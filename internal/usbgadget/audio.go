package usbgadget

var audioConfig = gadgetConfigItem{
	order:      2500,
	device:     "uac1.usb0",
	path:       []string{"functions", "uac1.usb0"},
	configPath: []string{"uac1.usb0"},
	attrs: gadgetAttributes{
		// The name the host shows for the audio device, the kernel's own
		// default being "Source/Sink"; loadGadgetConfig sets it to the product
		// name. The kernel drops the last character of a write without a
		// trailing newline.
		"function_name": "JetKVM USB Emulation Device\n",
		"c_chmask":      "3",
		"c_srate":       "48000",
		"c_ssize":       "2",
		"p_chmask":      "3",
		"p_srate":       "48000",
		"p_ssize":       "2",
		// Stream raw PCM without applying the gadget's ALSA mixer controls.
		// Hosts can apply software volume instead. Disable both feature units
		// to avoid a status IN endpoint and keep mass storage below the
		// RV1106 EP6 IN FIFO-reset bug on existing system images (#1634).
		"c_mute_present":   "0",
		"c_volume_present": "0",
		"p_mute_present":   "0",
		"p_volume_present": "0",
	},
}
