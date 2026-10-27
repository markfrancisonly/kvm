import { useCallback } from "react";

import { useHostAgentStore, useRTCStore } from "@/hooks/stores";
import useKeyboard from "@/hooks/useKeyboard";
import useKeyboardLayout from "@/hooks/useKeyboardLayout";
import { textToMacroSteps } from "@/keyboardPaste";
import { useJsonRpc } from "@/hooks/useJsonRpc";
import {
  blobDataPayload,
  blobIdPayload,
  decodeFrame,
  encodeFrame,
  FrameType,
  parseAck,
} from "@/hostagent/frame";


export interface PasteResult {
  via: "agent" | "keystrokes" | "cancelled";
  // Characters the keyboard layout cannot produce; empty via the agent.
  skipped: string[];
}

// Errors after which nothing reached the target, so typing it instead is safe.
const fallbackErrors = new Set(["no_agent", "disabled", "no_channel"]);
// The device sends text in frames of at most 60 KiB; a text that fits one
// frame is all or nothing, so "elevated" (the agent could not inject) is
// safe to retype for it. Longer texts may have been partly delivered.
const singleFrameChars = 15000;

// Pastes run one after another: a second keystroke macro cancels the one
// still typing, which truncated text that arrived in pieces. Cancelling
// bumps the generation so pastes queued before it are skipped.
let pasteQueue: Promise<unknown> = Promise.resolve();
let pasteGeneration = 0;
const AGENT_REPLY_TIMEOUT_MS = 30000;

const BLOB_CHUNK = 16 * 1024;
const BLOB_ACK_TIMEOUT_MS = 10000;
let blobSeq = 0;
let blobId = 0;

// Streams one file to the host agent through the device: begin, chunks and
// end, each acknowledged before the next is sent.
async function sendBlob(
  channel: RTCDataChannel,
  file: File,
  onProgress?: (sent: number, total: number) => void,
) {
  const id = ++blobId;
  const waiters = new Map<number, { resolve: () => void; reject: (e: Error) => void }>();
  const onMessage = (e: MessageEvent) => {
    const frame = decodeFrame(new Uint8Array(e.data as ArrayBuffer));
    if (!frame || (frame.type !== FrameType.Ack && frame.type !== FrameType.Nack)) return;
    const ack = parseAck(frame.payload);
    const waiter = ack && waiters.get(ack.seq);
    if (!ack || !waiter) return;
    waiters.delete(ack.seq);
    if (frame.type === FrameType.Ack) waiter.resolve();
    else waiter.reject(new Error(ack.note || "refused"));
  };
  channel.addEventListener("message", onMessage);

  const request = (type: number, payload: Uint8Array) =>
    new Promise<void>((resolve, reject) => {
      // Bit 31 is reserved for frames the device originates.
      const seq = ++blobSeq & 0x7fffffff;
      const timer = setTimeout(() => {
        waiters.delete(seq);
        reject(new Error("timeout"));
      }, BLOB_ACK_TIMEOUT_MS);
      waiters.set(seq, {
        resolve: () => {
          clearTimeout(timer);
          resolve();
        },
        reject: error => {
          clearTimeout(timer);
          reject(error);
        },
      });
      channel.send(encodeFrame({ type, seq, payload }));
    });

  try {
    const meta = {
      id,
      kind: file.type.startsWith("image/") ? "image" : "file",
      name: file.name || "paste",
      mime: file.type,
      size: file.size,
      paste: true,
    };
    await request(FrameType.BlobBegin, new TextEncoder().encode(JSON.stringify(meta)));
    for (let offset = 0; offset < file.size; offset += BLOB_CHUNK) {
      const bytes = new Uint8Array(await file.slice(offset, offset + BLOB_CHUNK).arrayBuffer());
      await request(FrameType.BlobData, blobDataPayload(id, offset, bytes));
      onProgress?.(offset + bytes.length, file.size);
    }
    await request(FrameType.BlobEnd, blobIdPayload(id));
  } catch (error) {
    try {
      channel.send(
        encodeFrame({
          type: FrameType.BlobCancel,
          seq: ++blobSeq & 0x7fffffff,
          payload: blobIdPayload(id),
        }),
      );
    } catch {
      // The channel is gone; the device cancels the blob itself.
    }
    throw error;
  } finally {
    channel.removeEventListener("message", onMessage);
  }
}

// Text and files go to the target through the host agent when one is
// present; text falls back to key presses of the selected layout.
export default function usePaste() {
  const { send } = useJsonRpc();
  const { executeMacro, cancelExecuteMacro } = useKeyboard();
  const { selectedKeyboard } = useKeyboardLayout();
  const agentPresent = useHostAgentStore(state => state.status.present);
  const rpcDataChannel = useRTCStore(state => state.rpcDataChannel);
  const hostAgentChannel = useRTCStore(state => state.hostAgentChannel);

  const pasteViaAgent = useCallback(
    (text: string) =>
      new Promise<void>((resolve, reject) => {
        if (rpcDataChannel?.readyState !== "open") {
          reject(new Error("no_channel"));
          return;
        }
        // A reply lost on the way must not hold the paste queue forever.
        const timer = setTimeout(() => reject(new Error("partial")), AGENT_REPLY_TIMEOUT_MS);
        void send("pasteText", { text }, resp => {
          clearTimeout(timer);
          if ("error" in resp) reject(new Error(String(resp.error.data ?? resp.error.message)));
          else resolve();
        });
      }),
    [send, rpcDataChannel],
  );

  const doPasteText = useCallback(
    async (text: string, options?: { delay?: number }): Promise<PasteResult> => {
      if (agentPresent) {
        try {
          await pasteViaAgent(text);
          return { via: "agent", skipped: [] };
        } catch (error) {
          const reason = error instanceof Error ? error.message : String(error);
          const safeToType =
            fallbackErrors.has(reason) ||
            (reason === "elevated" && text.length <= singleFrameChars);
          if (!safeToType) throw error;
          console.warn(`Host agent paste failed (${reason}), typing instead`);
        }
      }
      const { steps, skipped } = textToMacroSteps(text, selectedKeyboard, options?.delay);
      if (steps.length > 0) await executeMacro(steps);
      return { via: "keystrokes", skipped };
    },
    [agentPresent, pasteViaAgent, selectedKeyboard, executeMacro],
  );

  const pasteText = useCallback(
    (text: string, options?: { delay?: number }): Promise<PasteResult> => {
      const generation = pasteGeneration;
      const run = pasteQueue.then(() =>
        generation === pasteGeneration
          ? doPasteText(text, options)
          : { via: "cancelled" as const, skipped: [] },
      );
      pasteQueue = run.catch(() => undefined);
      return run;
    },
    [doPasteText],
  );

  // Stops the paste being typed and drops the ones queued behind it.
  const cancelPaste = useCallback(async () => {
    pasteGeneration++;
    await cancelExecuteMacro();
  }, [cancelExecuteMacro]);

  const pasteFiles = useCallback(
    async (files: File[], onProgress?: (sent: number, total: number) => void) => {
      if (!agentPresent) throw new Error("no_agent");
      if (hostAgentChannel?.readyState !== "open") throw new Error("no_channel");
      for (const file of files) await sendBlob(hostAgentChannel, file, onProgress);
    },
    [agentPresent, hostAgentChannel],
  );

  return { pasteText, pasteFiles, cancelPaste, agentPresent };
}
