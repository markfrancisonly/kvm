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

func TestAudioFunctionIsNamedAfterTheProduct(t *testing.T) {
	audio := audioConfig
	audio.attrs = gadgetAttributes{}
	u := &UsbGadget{configMap: map[string]gadgetConfigItem{
		"base":      {attrs: gadgetAttributes{}},
		"base_info": {attrs: gadgetAttributes{}},
		"audio":     audio,
	}}
	// The kernel drops the last character of a write without a trailing
	// newline, and its name field holds 31 characters.
	cases := map[string]string{
		"Wireless MultiMedia Keyboard":         "Wireless MultiMedia Keyboard\n",
		"A product name longer than the field": "A product name longer than the \n",
		"":                                     "USB Audio\n",
	}
	for product, want := range cases {
		u.customConfig = Config{Product: product}
		u.loadGadgetConfig()
		if got := u.configMap["audio"].attrs["function_name"]; got != want {
			t.Fatalf("%q: function_name = %q, want %q", product, got, want)
		}
	}
	if audioConfig.attrs["function_name"] != "JetKVM USB Emulation Device\n" {
		t.Fatal("the stock configuration should carry the stock product name")
	}
}
