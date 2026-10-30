package usbgadget

import "testing"

func TestAudioGadgetConfigFollowsEnabledDevice(t *testing.T) {
	u := &UsbGadget{enabledDevices: Devices{Audio: false}}
	if u.isGadgetConfigItemEnabled("audio") {
		t.Fatal("audio gadget should be disabled when audio device is disabled")
	}

	u.enabledDevices.Audio = true
	if !u.isGadgetConfigItemEnabled("audio") {
		t.Fatal("audio gadget should be enabled when audio device is enabled")
	}
}

func TestBaseGadgetConfigItemsAlwaysEnabled(t *testing.T) {
	u := &UsbGadget{}
	for _, item := range []string{"base", "base_info", "wake_hid"} {
		if !u.isGadgetConfigItemEnabled(item) {
			t.Fatalf("%s should always be enabled", item)
		}
	}
}

func TestAudioFunctionHasAFriendlyName(t *testing.T) {
	// The kernel drops the last character of a function_name written without
	// a trailing newline, and its own default "Source/Sink" reads like a fault.
	name := audioConfig.attrs["function_name"]
	if name == "" || name[len(name)-1] != '\n' {
		t.Fatalf("function_name must end in a newline, got %q", name)
	}
	if name == "Source/Sink\n" {
		t.Fatal("function_name should override the kernel default")
	}
}
