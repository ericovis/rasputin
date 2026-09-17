package card

import "testing"

// The two fixtures below are transcripts of the shape
// `diskutil list -plist external physical` and `diskutil info -plist disk4`
// print on macOS, converted the way List converts them
// (`plutil -convert json -o - -`). The values are a card reader with a 32 GB
// card in it; the keys are diskutil's.

const listJSON = `{
  "AllDisks": ["disk4", "disk4s1", "disk4s2"],
  "AllDisksAndPartitions": [
    {
      "Content": "FDisk_partition_scheme",
      "DeviceIdentifier": "disk4",
      "Partitions": [
        {"Content": "Windows_FAT_32", "DeviceIdentifier": "disk4s1", "Size": 536870912, "VolumeName": "bootfs"},
        {"Content": "Linux", "DeviceIdentifier": "disk4s2", "Size": 4294967296}
      ],
      "Size": 31914983424
    }
  ],
  "VolumesFromDisks": ["bootfs"],
  "WholeDisks": ["disk4"]
}`

const infoJSON = `{
  "Bootable": false,
  "BusProtocol": "USB",
  "DeviceIdentifier": "disk4",
  "DeviceNode": "/dev/disk4",
  "Ejectable": true,
  "Internal": false,
  "MediaName": "SD Card Reader",
  "RemovableMedia": true,
  "RemovableMediaOrExternalDevice": true,
  "Size": 31914983424,
  "TotalSize": 31914983424,
  "VirtualOrPhysical": "Physical",
  "WholeDisk": true,
  "Writable": true
}`

func TestWholeDisksReadsTheDiskIdentifiers(t *testing.T) {
	got, err := wholeDisks([]byte(listJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "disk4" {
		t.Errorf("wholeDisks = %v, want [disk4]", got)
	}
}

func TestWholeDisksOnAnEmptyListing(t *testing.T) {
	got, err := wholeDisks([]byte(`{"AllDisks": [], "WholeDisks": []}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("wholeDisks = %v, want none", got)
	}
}

func TestDeviceReadsWhatThePickerShows(t *testing.T) {
	got, ok := device([]byte(infoJSON))
	if !ok {
		t.Fatal("device refused a removable external card reader")
	}
	want := Device{Path: "/dev/disk4", RawPath: "/dev/rdisk4", Size: 31914983424, Name: "SD Card Reader", Removable: true}
	if got != want {
		t.Errorf("device = %+v, want %+v", got, want)
	}
}

// TestDeviceNeverOffersThisMachinesOwnDisk: the picker erases whatever is
// chosen from it, so an internal disk must not be one keystroke away —
// whichever of diskutil's several ways of saying so is the one it uses.
func TestDeviceNeverOffersThisMachinesOwnDisk(t *testing.T) {
	for name, doc := range map[string]string{
		"internal": `{"DeviceIdentifier":"disk3","DeviceNode":"/dev/disk3","Internal":true,"WholeDisk":true,"TotalSize":1000000000000}`,
		"disk0":    `{"DeviceIdentifier":"disk0","DeviceNode":"/dev/disk0","WholeDisk":true,"TotalSize":1000000000000}`,
		"slice":    `{"DeviceIdentifier":"disk4s2","DeviceNode":"/dev/disk4s2","WholeDisk":false,"TotalSize":4294967296}`,
		"virtual":  `{"DeviceIdentifier":"disk5","DeviceNode":"/dev/disk5","WholeDisk":true,"VirtualOrPhysical":"Virtual","TotalSize":4294967296}`,
		"garbage":  `not a document at all`,
	} {
		t.Run(name, func(t *testing.T) {
			if d, ok := device([]byte(doc)); ok {
				t.Errorf("device offered %+v", d)
			}
		})
	}
}

// TestDeviceToleratesRenamedKeys: diskutil has spelled "removable" several
// ways across macOS releases, and a key this code has not heard of must cost
// a word in one row, not the listing.
func TestDeviceToleratesRenamedKeys(t *testing.T) {
	got, ok := device([]byte(`{
	  "DeviceIdentifier": "disk4",
	  "WholeDisk": true,
	  "Ejectable": "Yes",
	  "IOKitSize": 31914983424
	}`))
	if !ok {
		t.Fatal("device refused a disk whose keys it only half recognises")
	}
	if got.Path != "/dev/disk4" || got.Size != 31914983424 || !got.Removable {
		t.Errorf("device = %+v", got)
	}
}
