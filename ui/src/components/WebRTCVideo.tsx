import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useResizeObserver } from "usehooks-ts";

import { cx } from "@/cva.config";
import { isWindows } from "@/utils";
import useGuestClipboard from "@hooks/useGuestClipboard";
import useGuestFileDrop from "@hooks/useGuestFileDrop";
import useKeyboard from "@hooks/useKeyboard";
import useMouse from "@hooks/useMouse";
import usePaste from "@hooks/usePaste";
import { isClientPasteShortcut, isInjectedPasteChord, isTypedCharacter } from "@/keyboardPaste";
import TargetTextInput, { targetTextInputId } from "@components/TargetTextInput";
import { useDevicePixelRatio } from "@hooks/useDevicePixelRatio";
import {
  RpcVideoState,
  useCapability,
  useRTCStore,
  useSettingsStore,
  useStorageDownloadStore,
  useUiStore,
  useUpdateStore,
  useVideoStore,
} from "@hooks/stores";
import { JsonRpcResponse, useJsonRpc } from "@hooks/useJsonRpc";
import VirtualKeyboard from "@components/VirtualKeyboard";
import Actionbar from "@components/ActionBar";
import MacroBar from "@components/MacroBar";
import InfoBar from "@components/InfoBar";
import {
  AudioPermissionBanner,
  HDMIErrorOverlay,
  LoadingConnectionOverlay,
  LoadingVideoOverlay,
  NoAutoplayPermissionsOverlay,
  PointerLockBar,
  VideoPausedOverlay,
} from "@components/VideoOverlay";
import OcrOverlay from "@components/OcrOverlay";
import { keys } from "@/keyboardMappings";
import notifications from "@/notifications";
import { m } from "@localizations/messages.js";

const initialHdmiErrorGraceMs = 2500;
// The device owns the switch between HDMI capture and OS Services video
// and reports a switching source until the new path has a stable signal
// (or its own give-up). Its worst case is ~38 s (parking wait + teardown +
// 30 s give-up); this longer timeout is only a safety net for a resolution
// event that never reached this client.
const sourceSwitchTimeoutMs = 45_000;

// On a device with OS Services, Ctrl+Shift+V and Ctrl+Shift+C (Cmd on macOS)
// move the clipboard between this browser and the target instead of reaching
// the target as keystrokes.
function isClipboardChord(e: KeyboardEvent) {
  return (
    (e.ctrlKey || e.metaKey) && e.shiftKey && !e.altKey && (e.code === "KeyV" || e.code === "KeyC")
  );
}

function isEditableTarget(target: EventTarget | null) {
  const element = target as HTMLElement | null;
  return (
    !!element &&
    (element.isContentEditable || element.tagName === "INPUT" || element.tagName === "TEXTAREA")
  );
}

function reportSkipped(skipped: string[]) {
  if (skipped.length > 0) notifications.error(m.paste_skipped_chars({ chars: skipped.join(" ") }));
}

export default function WebRTCVideo({
  hasConnectionIssues,
  hideStatusBar,
}: {
  hasConnectionIssues: boolean;
  hideStatusBar?: boolean;
}) {
  // Video and stream related refs and states
  const videoElm = useRef<HTMLVideoElement>(null);
  const audioElm = useRef<HTMLAudioElement>(null);
  const fullscreenContainerRef = useRef<HTMLDivElement>(null);
  const overlaysRef = useRef<HTMLDivElement>(null);
  const { mediaStream, mediaStreamTrackVersion, peerConnectionState } = useRTCStore();
  const [isPlaying, setIsPlaying] = useState(false);
  const [audioAutoplayBlocked, setAudioAutoplayBlocked] = useState(false);
  const [audioEnabled, setAudioEnabled] = useState(false);
  const [isPointerLockActive, setIsPointerLockActive] = useState(false);
  const [isKeyboardLockActive, setIsKeyboardLockActive] = useState(false);

  const { send: sendRpc } = useJsonRpc();

  const isPointerLockPossible =
    window.location.protocol === "https:" || window.location.hostname === "localhost";

  // Store hooks
  const settings = useSettingsStore();
  const { handleKeyPress, resetKeyboardState } = useKeyboard();
  const { pasteText } = usePaste();
  const {
    getRelMouseMoveHandler,
    getAbsMouseMoveHandler,
    getMouseWheelHandler,
    resetMousePosition,
  } = useMouse();
  const {
    setClientSize: setVideoClientSize,
    setSize: setVideoSize,
    width: videoWidth,
    height: videoHeight,
    clientWidth: videoClientWidth,
    clientHeight: videoClientHeight,
    hdmiState,
    videoSource,
    applyVideoState,
    setVideoElement,
    setContainerElement,
  } = useVideoStore();

  // Video enhancement settings
  const { videoSaturation, videoBrightness, videoContrast, videoScaling } = useSettingsStore();
  const devicePixelRatio = useDevicePixelRatio();

  // OCR mode
  const { isOcrMode, setPendingPasteText, setVideoContainer } = useUiStore();
  const { osServices, pasteViaGuest, copyFromGuest } = useGuestClipboard();
  // Files dragged from the desktop onto the video go to the remote host.
  useGuestFileDrop(videoElm);

  // The device page's focus trap uses the video container, so the hidden
  // text input inside it keeps the focus, in fullscreen too.
  useEffect(() => {
    setVideoContainer(fullscreenContainerRef.current);
    return () => setVideoContainer(null);
  }, [setVideoContainer]);

  // RTC related states
  const { peerConnection } = useRTCStore();

  // HDMI and UI states
  const isVideoLoading = !isPlaying;
  // Only a device with OS Services switches the video source.
  const isSourceSwitching =
    osServices && (videoSource === "switching_to_guest" || videoSource === "switching_to_physical");
  const [sourceSwitchTimedOut, setSourceSwitchTimedOut] = useState(false);
  const showSourceSwitchOverlay = isSourceSwitching && !sourceSwitchTimedOut;
  const rawHdmiError = ["no_lock", "no_signal", "out_of_range"].includes(hdmiState);
  const [isInitialHdmiErrorGraceActive, setIsInitialHdmiErrorGraceActive] = useState(false);
  const hdmiError = rawHdmiError && !isInitialHdmiErrorGraceActive;

  // A device without video_during_update pauses the stream once the
  // firmware download starts (systemUpdatePending) until it reboots.
  const videoDuringUpdate = useCapability("video_during_update");
  const { otaState } = useUpdateStore();
  const updatePausesVideo = !videoDuringUpdate && otaState.updating && otaState.systemUpdatePending;
  // A device with removable storage also pauses the stream while it
  // downloads a file to its TF card, such as the OS Services installer, so
  // the transfer gets the link.
  const downloadPausesVideo = useStorageDownloadStore(
    state => state.download?.state === "downloading" && state.download.videoPaused,
  );

  // Video-related
  const handleResize = useCallback(
    ({ width, height }: { width: number | undefined; height: number | undefined }) => {
      if (!videoElm.current) return;
      // Do something with width and height, e.g.:
      setVideoClientSize(width || 0, height || 0);
      setVideoSize(videoElm.current.videoWidth, videoElm.current.videoHeight);
    },
    [setVideoClientSize, setVideoSize],
  );

  // AltGr Fix for Windows Clients
  const altGrSyntheticThresholdMs = 3;
  const isWindowsClient = useMemo(() => isWindows(), []);
  const lastKeyDownRef = useRef<{ hidKey: number; time: number } | null>(null);
  const altGrLoopRef = useRef(false);

  useResizeObserver({
    ref: videoElm as React.RefObject<HTMLElement>,
    onResize: handleResize,
  });

  const updateVideoSizeStore = useCallback(
    (videoElm: HTMLVideoElement) => {
      setVideoClientSize(videoElm.clientWidth, videoElm.clientHeight);
      setVideoSize(videoElm.videoWidth, videoElm.videoHeight);
    },
    [setVideoClientSize, setVideoSize],
  );

  const onVideoPlaying = useCallback(() => {
    setIsPlaying(true);
    if (videoElm.current) updateVideoSizeStore(videoElm.current);
  }, [updateVideoSizeStore]);

  // Restoring EDID for idle display hiding cycles HDMI hotplug on the bridge.
  // The host can report no_signal/no_lock while it re-enumerates the display,
  // so keep the startup UI in the loading state before showing a persistent
  // HDMI error.
  useEffect(() => {
    if (peerConnectionState !== "connected" || isPlaying) {
      setIsInitialHdmiErrorGraceActive(false);
      return;
    }

    setIsInitialHdmiErrorGraceActive(true);
    const timeout = window.setTimeout(() => {
      setIsInitialHdmiErrorGraceActive(false);
    }, initialHdmiErrorGraceMs);

    return () => {
      window.clearTimeout(timeout);
    };
  }, [isPlaying, peerConnectionState]);

  // Safety net: the device resolves every switch, so reaching this timeout
  // means the resolution event never arrived here (lost to session churn or
  // a reload race). Resync from the device instead of trusting stale state,
  // and only report a failure if the device really is still switching.
  useEffect(() => {
    if (!isSourceSwitching) {
      setSourceSwitchTimedOut(false);
      return;
    }
    const timeout = window.setTimeout(() => {
      setSourceSwitchTimedOut(true);
      sendRpc("getVideoState", {}, (resp: JsonRpcResponse) => {
        if ("error" in resp) {
          notifications.error(m.video_overlay_source_switch_timeout());
          return;
        }
        const state = resp.result as RpcVideoState;
        applyVideoState(state);
        if (state.source === "switching_to_guest" || state.source === "switching_to_physical") {
          notifications.error(m.video_overlay_source_switch_timeout());
        }
      });
    }, sourceSwitchTimeoutMs);
    return () => window.clearTimeout(timeout);
  }, [isSourceSwitching, videoSource, sendRpc, applyVideoState]);

  // On mount, get the video size
  useEffect(
    function updateVideoSizeOnMount() {
      if (videoElm.current) updateVideoSizeStore(videoElm.current);
    },
    [updateVideoSizeStore],
  );

  // Store video and container element references
  useEffect(
    function storeVideoElementRef() {
      setVideoElement(videoElm.current);
      setContainerElement(fullscreenContainerRef.current);
      return () => {
        setVideoElement(null);
        setContainerElement(null);
      };
    },
    [setVideoElement, setContainerElement],
  );

  // Pointer lock and keyboard lock related
  const isFullscreenEnabled = document.fullscreenEnabled;

  const checkNavigatorPermissions = useCallback(async (permissionName: string) => {
    if (!navigator || !navigator.permissions || !navigator.permissions.query) {
      return false; // if can't query permissions, assume NOT granted
    }

    try {
      const name = permissionName as PermissionName;
      const { state } = await navigator.permissions.query({ name });
      return state === "granted";
    } catch {
      // ignore errors
    }
    return false; // if query fails, assume NOT granted
  }, []);

  const requestPointerLock = useCallback(async () => {
    if (!isPointerLockPossible || videoElm.current === null || document.pointerLockElement) return;

    const isPointerLockGranted = await checkNavigatorPermissions("pointer-lock");

    if (isPointerLockGranted && settings.mouseMode === "relative") {
      try {
        await videoElm.current.requestPointerLock();
      } catch {
        // ignore errors
      }
    }
  }, [checkNavigatorPermissions, isPointerLockPossible, settings.mouseMode]);

  const requestKeyboardLock = useCallback(async () => {
    if (videoElm.current === null) return;

    const isKeyboardLockGranted = await checkNavigatorPermissions("keyboard-lock");

    if (isKeyboardLockGranted && navigator && "keyboard" in navigator) {
      try {
        // @ts-expect-error - keyboard lock is not supported in all browsers
        await navigator.keyboard.lock();
        setIsKeyboardLockActive(true);
      } catch {
        // ignore errors
      }
    }
  }, [checkNavigatorPermissions, setIsKeyboardLockActive]);

  const releaseKeyboardLock = useCallback(async () => {
    if (
      fullscreenContainerRef.current === null ||
      document.fullscreenElement !== fullscreenContainerRef.current
    )
      return;

    if (navigator && "keyboard" in navigator) {
      try {
        // @ts-expect-error - keyboard unlock is not supported in all browsers
        await navigator.keyboard.unlock();
      } catch {
        // ignore errors
      }
      setIsKeyboardLockActive(false);
    }
  }, [setIsKeyboardLockActive]);

  useEffect(() => {
    if (!isPointerLockPossible || !videoElm.current) return;

    const handlePointerLockChange = () => {
      if (document.pointerLockElement) {
        notifications.success(m.video_pointer_lock_enabled());
        setIsPointerLockActive(true);
      } else {
        notifications.success(m.video_pointer_lock_disabled());
        setIsPointerLockActive(false);
      }
    };

    const abortController = new AbortController();
    const signal = abortController.signal;

    document.addEventListener("pointerlockchange", handlePointerLockChange, { signal });

    return () => {
      abortController.abort();
    };
  }, [isPointerLockPossible]);

  const requestFullscreen = useCallback(async () => {
    if (!isFullscreenEnabled || !fullscreenContainerRef.current) return;

    // per https://wicg.github.io/keyboard-lock/#system-key-press-handler
    // If keyboard lock is activated after fullscreen is already in effect, then the user my
    // see multiple messages about how to exit fullscreen. For this reason, we recommend that
    // developers call lock() before they enter fullscreen:
    await requestKeyboardLock();
    await requestPointerLock();

    await fullscreenContainerRef.current.requestFullscreen({
      navigationUI: "show",
    });
  }, [isFullscreenEnabled, requestKeyboardLock, requestPointerLock]);

  // setup to release the keyboard lock anytime the fullscreen ends
  useEffect(() => {
    if (!videoElm.current) return;

    const handleFullscreenChange = () => {
      if (!document.fullscreenElement) {
        releaseKeyboardLock();
      }
    };

    document.addEventListener("fullscreenchange", handleFullscreenChange);
  }, [releaseKeyboardLock]);

  const absMouseMoveHandler = useMemo(
    () =>
      getAbsMouseMoveHandler({
        videoClientWidth,
        videoClientHeight,
        videoWidth,
        videoHeight,
      }),
    [getAbsMouseMoveHandler, videoClientWidth, videoClientHeight, videoWidth, videoHeight],
  );

  const relMouseMoveHandler = useMemo(() => getRelMouseMoveHandler(), [getRelMouseMoveHandler]);

  const mouseWheelHandler = useMemo(() => getMouseWheelHandler(), [getMouseWheelHandler]);

  function getAdjustedKeyCode(e: KeyboardEvent) {
    const key = e.key;
    let code = e.code;

    if (code == "IntlBackslash" && ["`", "~"].includes(key)) {
      code = "Backquote";
    } else if (code == "Backquote" && ["§", "±"].includes(key)) {
      code = "IntlBackslash";
    }
    // For Japanese 106/109
    else if (code === "IntlYen") {
      code = "Yen";
    } else if (code === "IntlRo") {
      code = "KeyRO";
    } else if (code === "Convert") {
      code = "Henkan";
    } else if (code === "NonConvert") {
      code = "Muhenkan";
    } else if (key === "Shift" && code === "") {
      // Microsoft IME fix
      code = "ShiftRight";
    }

    return code;
  }

  // A click or key on the video is a user gesture: use it to start audio when
  // the autoplay policy blocked it, instead of asking for a second click.
  const retryBlockedAudio = useCallback(() => {
    if (!audioAutoplayBlocked) return;
    audioElm.current
      ?.play()
      .then(() => setAudioAutoplayBlocked(false))
      .catch(() => {});
  }, [audioAutoplayBlocked]);

  // Characters typed without a key code are collected briefly and pasted as text.
  const typedText = useRef({ text: "", timer: 0 });
  const queueTypedText = useCallback(
    (text: string) => {
      typedText.current.text += text;
      window.clearTimeout(typedText.current.timer);
      typedText.current.timer = window.setTimeout(() => {
        const pending = typedText.current.text;
        typedText.current.text = "";
        void pasteText(pending)
          .then(result => reportSkipped(result.skipped))
          .catch((error: unknown) =>
            notifications.error(m.paste_modal_failed_paste({ error: String(error) })),
          );
      }, 30);
    },
    [pasteText],
  );

  const focusTextInput = useCallback(() => {
    document.getElementById(targetTextInputId)?.focus({ preventScroll: true });
  }, []);

  // Take the focus once the video plays, unless an input already has it.
  useEffect(() => {
    if (!isPlaying) return;
    const active = document.activeElement;
    if (!active || active === document.body || active === videoElm.current) focusTextInput();
  }, [isPlaying, focusTextInput]);

  // Set by the Ctrl+Shift+V chord of a device with OS Services while its paste
  // switch is off: without an OS Services clipboard that paste opens the paste
  // popover. Every other paste is typed at once.
  const pasteOpensPopover = useRef(false);

  const keyDownHandler = useCallback(
    (e: KeyboardEvent) => {
      if (isOcrMode) return; // Let OCR overlay handle keys
      retryBlockedAudio();
      if (osServices && isClipboardChord(e) && e.code === "KeyC") {
        // Ctrl+Shift+C copies from the target and never reaches it; a held
        // chord copies once.
        e.preventDefault();
        if (!e.repeat) void copyFromGuest();
        return;
      }
      // A paste chord keeps its default action so the browser fires `paste`
      // (preventDefault would stop it) and the V does not reach the target;
      // pasteHandler does the rest. Ctrl+Shift+V is one on a device with OS
      // Services, and Ctrl+V or Ctrl+Shift+V when its switch is on. A held
      // chord pastes once: cancelling the repeat also cancels its `paste`.
      const pasteSwitch = isClientPasteShortcut(e, settings.pasteOnCtrlV, settings.pasteOnCtrlShiftV);
      if (pasteSwitch || (osServices && isClipboardChord(e))) {
        if (e.repeat) e.preventDefault();
        else pasteOpensPopover.current = !pasteSwitch;
        return;
      }
      if (isInjectedPasteChord(e)) {
        pasteOpensPopover.current = false;
        return;
      }
      e.preventDefault();
      const code = getAdjustedKeyCode(e);
      const hidKey = keys[code];

      if (hidKey === undefined) {
        if (isTypedCharacter(e)) {
          queueTypedText(e.key === "Enter" ? "\n" : e.key);
          return;
        }
        console.warn(`Key down not mapped: ${code}`);
        return;
      }

      // Detect Windows synthetic AltGr (CtrlLeft then AltRight within ~3ms) and cancel the synthetic Ctrl
      if (isWindowsClient) {
        // Buffer ControlLeft briefly; if no AltRight follows within the threshold, treat it as a real ControlLeft press.
        if (hidKey === keys.ControlLeft) {
          const controlLeftDownTime = e.timeStamp;
          lastKeyDownRef.current = { hidKey, time: controlLeftDownTime };
          setTimeout(() => {
            if (
              lastKeyDownRef.current?.hidKey === keys.ControlLeft &&
              lastKeyDownRef.current.time === controlLeftDownTime
            ) {
              lastKeyDownRef.current = null;
              handleKeyPress(keys.ControlLeft, true);
            }
          }, altGrSyntheticThresholdMs);
          return;
        }

        // If AltRight arrives shortly after ControlLeft, treat the pair as AltGr and cancel the pending ControlLeft.
        if (
          hidKey === keys.AltRight &&
          lastKeyDownRef.current?.hidKey === keys.ControlLeft &&
          e.timeStamp - lastKeyDownRef.current.time <= altGrSyntheticThresholdMs
        ) {
          altGrLoopRef.current = true;
          lastKeyDownRef.current = null;
        }

        // Microsoft IME fix:
        // Effective keydown events are consumed by IME (reported as "Process"),
        // so we handle the full press/release cycle in the keyup handler instead.
        if (["Zenkaku", "Hankaku", "ZenkakuHankaku"].includes(e.key)) {
          return;
        }
      }

      // When pressing the meta key + another key, the key will never trigger a keyup
      // event, so we need to clear the keys after a short delay
      // https://bugs.chromium.org/p/chromium/issues/detail?id=28089
      // https://bugzilla.mozilla.org/show_bug.cgi?id=1299553
      if (e.metaKey && hidKey < 0xe0) {
        setTimeout(() => {
          console.debug(`Forcing the meta key release of associated key: ${hidKey}`);
          handleKeyPress(hidKey, false);
        }, 10);
      }
      console.debug(`Key down: ${hidKey}`);
      handleKeyPress(hidKey, true);

      if (!isKeyboardLockActive && hidKey === keys.MetaLeft) {
        // If the left meta key was just pressed and we're not keyboard locked
        // we'll never see the keyup event because the browser is going to lose
        // focus so set a deferred keyup after a short delay
        setTimeout(() => {
          console.debug(`Forcing the left meta key release`);
          handleKeyPress(hidKey, false);
        }, 100);
      }
    },
    [
      handleKeyPress,
      isKeyboardLockActive,
      isOcrMode,
      isWindowsClient,
      retryBlockedAudio,
      osServices,
      copyFromGuest,
      settings.pasteOnCtrlV,
      settings.pasteOnCtrlShiftV,
      queueTypedText,
    ],
  );

  const keyUpHandler = useCallback(
    async (e: KeyboardEvent) => {
      if (isOcrMode) return; // Let OCR overlay handle keys
      // The clipboard and paste chords never reached the target on keydown.
      if (osServices && isClipboardChord(e)) return;
      if (isClientPasteShortcut(e, settings.pasteOnCtrlV, settings.pasteOnCtrlShiftV)) return;
      if (isTypedCharacter(e)) return;
      e.preventDefault();
      const code = getAdjustedKeyCode(e);
      const hidKey = keys[code];

      if (hidKey === undefined) {
        console.warn(`Key up not mapped: ${code}`);
        return;
      }

      if (isWindowsClient) {
        // On Windows, handle ControlLeft specially to preserve FIFO semantics with AltGr buffering.
        if (hidKey === keys.ControlLeft) {
          // Synthetic AltGr ControlLeft: never sent a down, swallow the release as well.
          if (altGrLoopRef.current) {
            altGrLoopRef.current = false;
            return;
          }

          // Very fast real Ctrl tap: flush the pending down before the up.
          if (lastKeyDownRef.current?.hidKey === keys.ControlLeft) {
            handleKeyPress(keys.ControlLeft, true);
          }

          lastKeyDownRef.current = null;
        }

        // Microsoft IME fix:
        // Synthesize the missing keydown event to ensure a complete key press cycle.
        if (["Zenkaku", "Hankaku", "ZenkakuHankaku"].includes(e.key)) {
          console.debug(`Synthesizing missed key down for IME key: ${e.key}`);
          handleKeyPress(hidKey, true);
        }
      }

      console.debug(`Key up: ${hidKey}`);
      handleKeyPress(hidKey, false);
    },
    [
      handleKeyPress,
      isOcrMode,
      isWindowsClient,
      osServices,
      settings.pasteOnCtrlV,
      settings.pasteOnCtrlShiftV,
    ],
  );

  const videoKeyUpHandler = useCallback((e: KeyboardEvent) => {
    if (!videoElm.current) return;

    // In fullscreen mode in chrome & safari, the space key is used to pause/play the video
    // there is no way to prevent this, so we need to simply force play the video when it's paused.
    // Fix only works in chrome based browsers.
    if (e.code === "Space") {
      if (videoElm.current.paused) {
        console.debug("Force playing video");
        videoElm.current.play();
      }
    }
  }, []);

  const videoKeyDownHandler = useCallback((e: KeyboardEvent) => {
    // Prevent the browser's native <video> play/pause toggle on Space.
    // The document-level keyDownHandler already calls preventDefault, but
    // in fullscreen the video element receives the event first.
    if (e.code === "Space") {
      e.preventDefault();
    }
  }, []);

  const addStreamToVideoElm = useCallback(
    (mediaStream: MediaStream) => {
      if (!videoElm.current) return;
      const videoElmRefValue = videoElm.current;
      videoElmRefValue.srcObject = mediaStream;
      updateVideoSizeStore(videoElmRefValue);
    },
    [updateVideoSizeStore],
  );

  useEffect(
    function updateVideoStream() {
      // Each new peer connection brings a new MediaStream, so this is where isPlaying resets.
      // An ICE disconnect that recovers keeps the stream, and the video element keeps playing.
      setIsPlaying(false);

      if (!mediaStream) {
        if (videoElm.current) videoElm.current.srcObject = null;
        return;
      }

      addStreamToVideoElm(mediaStream);
    },
    [addStreamToVideoElm, mediaStream],
  );

  // Fetch the device's audio-enabled state once the RPC channel is ready.
  // We only attach the <audio> element below when it's true — otherwise
  // Firefox prompts for audio autoplay permission on a silent stream that
  // would never actually play any sound.
  useEffect(
    function fetchAudioConfig() {
      if (peerConnection?.connectionState !== "connected") return;
      sendRpc("getAudioConfig", {}, (resp: JsonRpcResponse) => {
        if ("error" in resp) return;
        setAudioEnabled((resp.result as { enabled: boolean }).enabled);
      });
    },
    [peerConnection?.connectionState, sendRpc],
  );

  // Audio plays through a separate <audio> element because the <video> is
  // muted (kept muted so video autoplay isn't blocked when no user gesture
  // has been recorded). If the browser blocks audio autoplay, a small banner
  // surfaces a click target while video playback and input continue.
  useEffect(
    function updateAudioStream() {
      const elm = audioElm.current;
      if (!elm || !mediaStream || !audioEnabled) return;

      elm.srcObject = mediaStream;
      elm
        .play()
        .then(() => setAudioAutoplayBlocked(false))
        // Only the autoplay policy needs a click; a play() interrupted by the
        // next stream change is not a permission problem.
        .catch((error: unknown) =>
          setAudioAutoplayBlocked((error as DOMException | undefined)?.name === "NotAllowedError"),
        );

      return () => {
        elm.srcObject = null;
        setAudioAutoplayBlocked(false);
      };
    },
    [mediaStream, mediaStreamTrackVersion, audioEnabled],
  );

  // Pastes this browser's clipboard into the target; inputs and popovers keep
  // theirs. A paste event comes from a paste chord left to its default action
  // above, from the browser's Edit menu, or from a dictation tool.
  //
  // While the video is used the hidden text input holds the focus, so the
  // paste lands there and that field types it into the target. A paste on the
  // document itself goes to an OS Services clipboard when the target has one.
  // Without one it is typed at once, except after the Ctrl+Shift+V chord with
  // its paste switch off, which opens the paste popover.
  const pasteHandler = useCallback(
    (e: ClipboardEvent) => {
      if (isOcrMode) return;
      const viaPopover = pasteOpensPopover.current;
      pasteOpensPopover.current = false;
      const target = e.target as HTMLElement | null;
      const isTextInputShim = target?.hasAttribute("data-text-input-shim") ?? false;
      // Text pasted into the hidden text input is left to the browser: the
      // field's input event forwards it, and the text stays there for a
      // moment because a dictation tool may read it back to confirm the paste
      // landed before it restores the clipboard.
      if (isTextInputShim) return;
      if (isEditableTarget(target) || target?.closest("[data-headlessui-state]")) return;
      e.preventDefault();
      const text = e.clipboardData?.getData("text/plain") ?? "";
      if (!text) {
        notifications.error(m.clipboard_paste_empty());
        return;
      }
      void pasteViaGuest(text)
        .then(result => {
          if (result !== "unavailable") return;
          if (viaPopover) {
            setPendingPasteText(text);
            return;
          }
          return pasteText(text).then(typed => reportSkipped(typed.skipped));
        })
        .catch((error: unknown) =>
          notifications.error(m.paste_modal_failed_paste({ error: String(error) })),
        );
    },
    [isOcrMode, pasteViaGuest, pasteText, setPendingPasteText],
  );

  // Setup Keyboard Events
  useEffect(
    function setupKeyboardEvents() {
      const abortController = new AbortController();
      const signal = abortController.signal;

      document.addEventListener("keydown", keyDownHandler, { signal });
      document.addEventListener("keyup", keyUpHandler, { signal });
      document.addEventListener("paste", pasteHandler, { signal });

      window.addEventListener("blur", resetKeyboardState, { signal });
      document.addEventListener("visibilitychange", resetKeyboardState, { signal });

      return () => {
        abortController.abort();
      };
    },
    [keyDownHandler, keyUpHandler, pasteHandler, resetKeyboardState],
  );

  // Setup Video Event Listeners
  useEffect(
    function setupVideoEventListeners() {
      const videoElmRefValue = videoElm.current;
      if (!videoElmRefValue) return;

      const abortController = new AbortController();
      const signal = abortController.signal;

      // To prevent the video from being paused when the user presses a space in fullscreen mode
      videoElmRefValue.addEventListener("keydown", videoKeyDownHandler, { signal });
      videoElmRefValue.addEventListener("keyup", videoKeyUpHandler, { signal });
      videoElmRefValue.addEventListener("mousedown", retryBlockedAudio, { signal });

      // A click on the video must not move the focus away from the hidden
      // text input (the default mousedown action would), so it stays there.
      videoElmRefValue.addEventListener(
        "mousedown",
        e => {
          e.preventDefault();
          focusTextInput();
        },
        { signal },
      );

      // We need to know when the video is playing to update state and video size
      videoElmRefValue.addEventListener("playing", onVideoPlaying, { signal });
      // A stream resolution change only fires resize, so the store size would go stale
      videoElmRefValue.addEventListener("resize", () => updateVideoSizeStore(videoElmRefValue), {
        signal,
      });

      return () => {
        abortController.abort();
      };
    },
    [
      onVideoPlaying,
      videoKeyDownHandler,
      videoKeyUpHandler,
      retryBlockedAudio,
      focusTextInput,
      updateVideoSizeStore,
    ],
  );

  // Setup Mouse Events
  useEffect(
    function setMouseModeEventListeners() {
      const videoElmRefValue = videoElm.current;
      if (!videoElmRefValue) return;

      const isRelativeMouseMode = settings.mouseMode === "relative";
      const mouseHandler = isRelativeMouseMode ? relMouseMoveHandler : absMouseMoveHandler;

      const abortController = new AbortController();
      const signal = abortController.signal;

      videoElmRefValue.addEventListener("mousemove", mouseHandler, { signal });
      videoElmRefValue.addEventListener("pointerdown", mouseHandler, { signal });
      videoElmRefValue.addEventListener("pointerup", mouseHandler, { signal });
      videoElmRefValue.addEventListener("wheel", mouseWheelHandler, {
        signal,
        passive: true,
      });

      if (isRelativeMouseMode) {
        containerRef.current?.addEventListener(
          "click",
          (e: MouseEvent) => {
            const target = e.target as Element;
            if (target !== videoElmRefValue) {
              const overlays = overlaysRef.current;
              if (!hdmiError || !overlays?.contains(target)) return;

              const isOnControl = Array.from(overlays.querySelectorAll("a, button")).some(el => {
                const r = el.getBoundingClientRect();
                return (
                  e.clientX >= r.left &&
                  e.clientX <= r.right &&
                  e.clientY >= r.top &&
                  e.clientY <= r.bottom
                );
              });
              if (isOnControl) return;
            }

            if (isPointerLockPossible && !isPointerLockActive && !document.pointerLockElement) {
              requestPointerLock();
            }
          },
          { signal },
        );
      } else {
        // Reset the mouse position when the window is blurred or the document is hidden
        window.addEventListener("blur", resetMousePosition, { signal });
        document.addEventListener("visibilitychange", resetMousePosition, { signal });
      }

      const preventContextMenu = (e: MouseEvent) => e.preventDefault();
      videoElmRefValue.addEventListener("contextmenu", preventContextMenu, { signal });

      // Suppress browser Back/Forward navigation on X1/X2 mouse buttons so
      // those presses are forwarded to the remote target instead.
      const preventXButtonNav = (e: MouseEvent) => {
        if (e.button === 3 || e.button === 4) e.preventDefault();
      };
      videoElmRefValue.addEventListener("mousedown", preventXButtonNav, { signal });
      videoElmRefValue.addEventListener("mouseup", preventXButtonNav, { signal });
      videoElmRefValue.addEventListener("auxclick", preventXButtonNav, { signal });

      return () => {
        abortController.abort();
      };
    },
    [
      hdmiError,
      isPointerLockActive,
      isPointerLockPossible,
      requestPointerLock,
      absMouseMoveHandler,
      relMouseMoveHandler,
      mouseWheelHandler,
      resetMousePosition,
      settings.mouseMode,
    ],
  );

  const containerRef = useRef<HTMLDivElement>(null);

  const hasNoAutoPlayPermissions = useMemo(() => {
    if (peerConnection?.connectionState !== "connected") return false;
    if (isPlaying) return false;
    if (hdmiError) return false;
    if (videoHeight === 0 || videoWidth === 0) return false;
    return true;
  }, [hdmiError, isPlaying, peerConnection?.connectionState, videoHeight, videoWidth]);

  const showPointerLockBar = useMemo(() => {
    if (settings.mouseMode !== "relative") return false;
    if (!isPointerLockPossible) return false;
    if (isPointerLockActive) return false;
    if (isVideoLoading) return false;
    if (!isPlaying) return false;
    if (videoHeight === 0 || videoWidth === 0) return false;
    return true;
  }, [
    isPlaying,
    isPointerLockActive,
    isPointerLockPossible,
    isVideoLoading,
    settings.mouseMode,
    videoHeight,
    videoWidth,
  ]);

  // Conditionally set the filter style so we don't fallback to software rendering if these values are default of 1.0
  const videoStyle = useMemo(() => {
    const style: React.CSSProperties = {};
    if (videoSaturation !== 1.0 || videoBrightness !== 1.0 || videoContrast !== 1.0) {
      style.filter = `saturate(${videoSaturation}) brightness(${videoBrightness}) contrast(${videoContrast})`;
    }
    if (videoScaling === "actual" && videoWidth && videoHeight) {
      style.width = videoWidth / devicePixelRatio;
      style.height = videoHeight / devicePixelRatio;
      style.maxHeight = "100%";
    } else if (videoWidth && videoHeight) {
      // Fit to window: as wide as the container allows, or as wide as its
      // height allows at the stream's aspect, whichever is smaller. The box
      // is then the picture itself, scaled up or down.
      style.width = `min(100%, calc(100cqh * ${videoWidth} / ${videoHeight}))`;
      style.aspectRatio = `${videoWidth} / ${videoHeight}`;
    }
    return style;
  }, [
    videoSaturation,
    videoBrightness,
    videoContrast,
    videoScaling,
    videoWidth,
    videoHeight,
    devicePixelRatio,
  ]);

  return (
    <div className="grid h-full w-full grid-rows-(--grid-layout)">
      <div className="flex min-h-[39.5px] flex-col">
        <div className="flex flex-col">
          <fieldset disabled={peerConnection?.connectionState !== "connected"} className="contents">
            <Actionbar requestFullscreen={requestFullscreen} />
            <MacroBar />
          </fieldset>
        </div>
      </div>

      <div ref={containerRef} className="h-full overflow-hidden">
        <div className="relative h-full">
          <div
            className={cx(
              "absolute inset-0 z-0 bg-blue-50/40 opacity-80 dark:bg-slate-800/40",
              "bg-[radial-gradient(var(--color-blue-300)_0.5px,transparent_0.5px),radial-gradient(var(--color-blue-300)_0.5px,transparent_0.5px)] dark:bg-[radial-gradient(var(--color-slate-700)_0.5px,transparent_0.5px),radial-gradient(var(--color-slate-700)_0.5px,transparent_0.5px)]",
              "bg-position-[0_0,10px_10px]",
              "bg-size-[20px_20px]",
            )}
          />
          <div className="flex h-full flex-col">
            <div className="relative grow overflow-hidden">
              <div className="flex h-full flex-col">
                <div className="grid grow grid-rows-(--grid-bodyFooter) overflow-hidden">
                  {/* In relative mouse mode and under https, we enable the pointer lock, and to do so we need a bar to show the user to click on the video to enable mouse control */}
                  <PointerLockBar show={showPointerLockBar} />
                  <div className="relative mx-4 my-2 flex items-center justify-center overflow-hidden">
                    <div
                      ref={fullscreenContainerRef}
                      className="relative flex h-full w-full items-center justify-center [container-type:size]"
                    >
                      {/* The element keeps the picture's own shape, so the pointer is only tracked over the picture. */}
                      <video
                        ref={videoElm}
                        autoPlay
                        controls={false}
                        onPlaying={onVideoPlaying}
                        onPlay={onVideoPlaying}
                        muted
                        playsInline
                        disablePictureInPicture
                        controlsList="nofullscreen"
                        style={videoStyle}
                        className={cx("max-h-full max-w-full transition-all duration-1000", {
                          "cursor-none": settings.isCursorHidden,
                          "pointer-events-none": isOcrMode,
                          "opacity-0!":
                            isVideoLoading ||
                            hdmiError ||
                            hasConnectionIssues ||
                            peerConnectionState !== "connected",
                          "opacity-60!": showPointerLockBar,
                          "animate-slideUpFade": isPlaying,
                        })}
                      />
                      {audioEnabled && <audio ref={audioElm} autoPlay playsInline hidden />}
                      <TargetTextInput />
                      <OcrOverlay />
                      {peerConnection?.connectionState == "connected" && !hasConnectionIssues && (
                        <div
                          style={{ animationDuration: "500ms" }}
                          className="pointer-events-none absolute inset-0 flex animate-slideUpFade items-center justify-center"
                        >
                          <div ref={overlaysRef} className="relative h-full w-full rounded-md">
                            <LoadingConnectionOverlay
                              show={showSourceSwitchOverlay}
                              text={
                                videoSource === "switching_to_physical"
                                  ? m.video_overlay_switching_to_physical()
                                  : m.video_overlay_switching_to_guest()
                              }
                            />
                            <LoadingVideoOverlay
                              show={isVideoLoading && !showSourceSwitchOverlay}
                            />
                            <HDMIErrorOverlay show={hdmiError} hdmiState={hdmiState} />
                            <VideoPausedOverlay
                              show={updatePausesVideo}
                              title={m.video_overlay_update_paused_title()}
                              description={m.video_overlay_update_paused_description()}
                            />
                            <VideoPausedOverlay
                              show={downloadPausesVideo && !updatePausesVideo}
                              title={m.video_overlay_download_paused_title()}
                              description={m.video_overlay_download_paused_description()}
                            />
                            <NoAutoplayPermissionsOverlay
                              show={hasNoAutoPlayPermissions}
                              onPlayClick={() => {
                                videoElm.current?.play();
                                audioElm.current
                                  ?.play()
                                  .then(() => setAudioAutoplayBlocked(false))
                                  .catch(() => undefined);
                              }}
                            />
                            <AudioPermissionBanner
                              show={isPlaying && audioEnabled && audioAutoplayBlocked && !hdmiError}
                              onEnableAudio={() => {
                                audioElm.current
                                  ?.play()
                                  .then(() => setAudioAutoplayBlocked(false))
                                  .catch(() => undefined);
                              }}
                            />
                          </div>
                        </div>
                      )}
                    </div>
                  </div>
                  <VirtualKeyboard />
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
      {!hideStatusBar && (
        <div>
          <InfoBar />
        </div>
      )}
    </div>
  );
}
