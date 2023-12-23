package labsscratch

import (
	"encoding/base64"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
	"tinygo.org/x/bluetooth"
)

func matchDevice(device bluetooth.ScanResult, filters []DiscoverFilter) bool {
	//TODO: implement match device

	// export function matchesFilter(device: Device, filter: Filter) {
	//   return (
	//     (filter.name === undefined ||
	//       device.Name?.value === filter.name ||
	//       device.Alias?.value === filter.name) &&
	//     (filter.namePrefix === undefined ||
	//       (device.Name?.value ?? "").startsWith(filter.namePrefix) ||
	//       (device.Alias?.value ?? "").startsWith(filter.namePrefix)) &&
	//     !filter.services?.some(
	//       (uuid) => !(device.UUIDs?.value ?? []).includes(uuid)
	//     ) &&
	//     (filter.manufacturerData === undefined ||
	//       (device.ManufacturerData &&
	//         !Object.entries(filter.manufacturerData).some(([id, value]) => {
	//           const buff = device.ManufacturerData!.value[id]?.value;

	//	          return (
	//	            !buff ||
	//	            value.mask.length > buff.length ||
	//	            value.mask.some(
	//	              (_, i) =>
	//	                (buff.readUInt8(i) & value.mask[i]) !== value.dataPrefix[i]
	//	            )
	//	          );
	//	        })))
	//	  );
	//	}

	for _, filter := range filters {
		if len(filter.Name) != 0 && filter.Name != device.LocalName() {
			return false
		}

		for _, service := range filter.Services {
			if !device.HasServiceUUID(bluetooth.NewUUID(service)) {
				return false
			}
		}
	}
	return true
}

func getDeviceCharacteristic(device bluetooth.Device, serviceId, characteristicId bluetooth.UUID) (bluetooth.DeviceCharacteristic, error) {
	services, err := device.DiscoverServices([]bluetooth.UUID{serviceId})
	if err != nil {
		return bluetooth.DeviceCharacteristic{}, err
	}

	chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{characteristicId})
	if err != nil {
		return bluetooth.DeviceCharacteristic{}, err
	}

	return chars[0], nil
}

func notificationCallback(c *websocket.Conn, ServiceId, CharacteristicId uuid.UUID) func(buf []byte) {
	return func(buf []byte) {
		_ = WsSend(c, NewMsg("characteristicDidChange", UpdateParams{
			ServiceId:        ServiceId,
			CharacteristicId: CharacteristicId,
			Message:          base64.StdEncoding.EncodeToString(buf),
			Encoding:         "base64",
		}))
	}
}
