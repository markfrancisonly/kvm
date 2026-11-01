import { useCallback, useEffect } from "react";

import useKeyboard from "@/hooks/useKeyboard";
import { JsonRpcResponse, useJsonRpc } from "@/hooks/useJsonRpc";
import { keyboardForLayout } from "@/hooks/useKeyboardLayout";
import { useSettingsStore } from "@/hooks/stores";
import { textToMacroSteps } from "@/keyboardPaste";

export interface PasteResult {
  via: "keystrokes" | "cancelled";
  // Characters the keyboard layout cannot produce.
  skipped: string[];
}

// Pastes run one after another: a second keystroke macro cancels the one
// still typing, which truncated text that arrived in pieces. Cancelling
// bumps the generation so pastes queued before it are skipped.
let pasteQueue: Promise<unknown> = Promise.resolve();
let pasteGeneration = 0;

// Only the keyboard settings and the paste modal fetched the device's layout,
// so a fresh browser typed its first dictation or shortcut paste with the
// "en-US" default. The layout is fetched once here and the first paste waits
// for it; a fetch that gets no answer, before the channel is open, is retried
// by the next paste.
let layoutFetched = false;
let layoutFetch: Promise<void> | null = null;

function loadKeyboardLayout(
  send: ReturnType<typeof useJsonRpc>["send"],
  setKeyboardLayout: (layout: string) => void,
): Promise<void> {
  if (layoutFetched) return Promise.resolve();
  if (!layoutFetch) {
    layoutFetch = new Promise<void>((resolve) => {
      const giveUp = window.setTimeout(() => {
        layoutFetch = null;
        resolve();
      }, 1500);
      void send("getKeyboardLayout", {}, (resp: JsonRpcResponse) => {
        window.clearTimeout(giveUp);
        if (!("error" in resp)) {
          setKeyboardLayout(resp.result as string);
          layoutFetched = true;
        }
        layoutFetch = null;
        resolve();
      });
    });
  }
  return layoutFetch;
}

// One paste path for the paste modal, the clipboard shortcuts and text
// inserted by dictation tools: the text is typed with the selected layout.
export default function usePaste() {
  const { executeMacro, cancelExecuteMacro } = useKeyboard();
  const { send } = useJsonRpc();
  const { setKeyboardLayout } = useSettingsStore();

  useEffect(() => {
    void loadKeyboardLayout(send, setKeyboardLayout);
  }, [send, setKeyboardLayout]);

  const doPasteText = useCallback(
    async (
      text: string,
      options?: { delay?: number },
    ): Promise<PasteResult> => {
      await loadKeyboardLayout(send, setKeyboardLayout);
      const keyboard = keyboardForLayout(
        useSettingsStore.getState().keyboardLayout,
      );
      const { steps, skipped } = textToMacroSteps(
        text,
        keyboard,
        options?.delay,
      );
      if (steps.length > 0) await executeMacro(steps);
      return { via: "keystrokes", skipped };
    },
    [send, setKeyboardLayout, executeMacro],
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

  return { pasteText, cancelPaste };
}
