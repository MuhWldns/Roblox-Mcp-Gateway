// Package bridgeapp — fingerprint_windows.go
// Derives a stable, hardware-bound device fingerprint hash on Windows from
// independent MachineGuid, SMBIOS UUID, and system-volume serial sources.
// At least two sources are required; callers omit the fingerprint when the
// machine cannot provide enough reliable identifiers.

package bridgeapp

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// MachineFingerprintHash returns the hex-encoded HMAC-SHA256 hardware digest
// sent alongside the persisted random installation id during enrollment.
func MachineFingerprintHash() (string, error) {
	return deriveMachineFingerprintHash(machineGUID(), smbiosUUID(), volumeSerial())
}

// machineGUID reads HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid.
func machineGUID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	val, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// smbiosUUID reads the SMBIOS Type 1 (System Information) UUID directly via
// GetSystemFirmwareTable, avoiding any WMI dependency.
func smbiosUUID() string {
	uuid, err := smbiosType1UUID()
	if err != nil {
		return ""
	}
	return uuid
}

func smbiosType1UUID() (string, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getFirmwareTable := kernel32.NewProc("GetSystemFirmwareTable")

	const rsmb = uintptr(0x52534D42)
	size, _, _ := getFirmwareTable.Call(rsmb, 0, 0, 0)
	if size == 0 {
		return "", fmt.Errorf("GetSystemFirmwareTable: empty table")
	}
	buf := make([]byte, size)
	ret, _, err := getFirmwareTable.Call(rsmb, 0,
		uintptr(unsafe.Pointer(&buf[0])), size)
	if ret == 0 {
		return "", fmt.Errorf("GetSystemFirmwareTable: %w", err)
	}

	// RSMB buffer: 4 bytes padding + 4 bytes length + data
	if len(buf) < 8 {
		return "", fmt.Errorf("SMBIOS buffer too short")
	}
	dataLen := binary.LittleEndian.Uint32(buf[4:8])
	if int(dataLen)+8 > len(buf) {
		return "", fmt.Errorf("SMBIOS data length exceeds buffer")
	}
	data := buf[8 : 8+dataLen]

	// Walk SMBIOS structures; each has a header (type, length, handle=2 bytes).
	for i := 0; i+4 <= len(data); {
		stype := data[i]
		slen := int(data[i+1])
		if slen < 4 || i+slen > len(data) {
			break
		}
		if stype == 1 && slen >= 0x19 {
			// UUID at offset 0x08 from structure start, 16 bytes.
			// Per SMBIOS 2.6+: bytes 0-3, 4-5, 6-7 are little-endian.
			raw := data[i+8 : i+24]
			return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
				raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
		}
		// Skip formatted area then unformatted (string) section.
		j := i + slen
		for j+1 < len(data) {
			if data[j] == 0 && data[j+1] == 0 {
				j += 2
				break
			}
			j++
		}
		i = j
	}
	return "", fmt.Errorf("SMBIOS Type 1 structure not found")
}

// volumeSerial reads the volume serial number of the system root.
func volumeSerial() string {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getVolumeInfo := kernel32.NewProc("GetVolumeInformationW")

	root, err := windows.UTF16PtrFromString(`C:\`)
	if err != nil {
		return ""
	}
	var serial uint32
	ret, _, _ := getVolumeInfo.Call(
		uintptr(unsafe.Pointer(root)),
		0, 0,
		uintptr(unsafe.Pointer(&serial)),
		0, 0, 0, 0,
	)
	if ret == 0 {
		return ""
	}
	return fmt.Sprintf("%08x", serial)
}
