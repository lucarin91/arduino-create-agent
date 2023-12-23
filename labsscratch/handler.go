package labsscratch

import (
	"encoding/base64"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/websocket"
	"tinygo.org/x/bluetooth"
)

func GetHandler(adapter *bluetooth.Adapter) websocket.Handler {
	return websocket.Handler(func(c *websocket.Conn) {
		log.Printf("client connected from %q\n", c.RemoteAddr())

		var DEVICE *bluetooth.Device

		msgs := make(chan Msg, 100)
		go WsReadLoop(c, msgs)

		for msg := range msgs {
			switch msg.Method {
			case "getVersion":
				_ = WsSend(c, msg.Respond(map[string]string{"protocol": "1.3"}))

			case "discover":
				params, err := DiscoverParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				// TODO: scan should be async
				err = adapter.Scan(func(adapter *bluetooth.Adapter, device bluetooth.ScanResult) {
					if len(device.LocalName()) == 0 {
						return
					}

					log.Debug("found device:", device.Address.String(), device.RSSI, device.LocalName())

					if !matchDevice(device, params.Filters) {
						return
					}

					if err := adapter.StopScan(); err != nil {
						log.Errorf("stop scan error: %s\n", err)
						return
					}

					msg := NewMsg("didDiscoverPeripheral", Device{
						PeripheralId: device.Address.String(),
						Name:         device.LocalName(),
						RSSI:         device.RSSI,
					})

					err := WsSend(c, msg)
					if err != nil {
						return
					}
				})
				if err != nil {
					log.Errorf("scan error: %s", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.Respond(nil))

			case "connect":
				params, err := ConnectParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				mac := bluetooth.Address{}
				mac.Set(params.PeripheralId)
				DEVICE, err = adapter.Connect(mac, bluetooth.ConnectionParams{
					ConnectionTimeout: 0,
					MinInterval:       0,
					MaxInterval:       0,
				})
				if err != nil {
					log.Errorf("ble connect error: %s", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.Respond(nil))

			case "startNotifications":
				params, err := NotificationsParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}
				log.Printf("startNotifications params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					log.Errorf("get device characteristic error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				err = char.EnableNotifications(notificationCallback(c, params.CharacteristicId, params.CharacteristicId))
				if err != nil {
					log.Errorf("enable notification error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.Respond(nil))

			case "write":
				params, err := UpdateParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}
				log.Printf("write params: %+v\n", params)

				if params.Encoding != "base64" {
					log.Errorf("encoding format %q not supported\n", params.Encoding)
					continue
				}

				services, err := DEVICE.DiscoverServices([]bluetooth.UUID{bluetooth.NewUUID(params.ServiceId)})
				if err != nil {
					log.Errorf("discover service error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{bluetooth.NewUUID(params.CharacteristicId)})
				if err != nil {
					log.Errorf("discovert characteristics error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}
				char := chars[0]

				buf, err := base64.StdEncoding.DecodeString(params.Message)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				// TODO: handle params.WithResponse
				n, err := char.WriteWithoutResponse(buf)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.Respond(n))

			case "read":
				params, err := ReadParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}
				log.Printf("read params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					log.Errorf("get device characteristic error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				if params.StartNotifications {
					err = char.EnableNotifications(notificationCallback(c, params.CharacteristicId, params.CharacteristicId))
					if err != nil {
						log.Errorf("enable notification error: %s\n", err)
						_ = WsSend(c, msg.Error(err.Error()))
						continue
					}
				}

				buf := make([]byte, 512)
				n, err := char.Read(buf)
				if err != nil {
					log.Errorf("read characteristic error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.RespondBytes(buf[:n]))

			case "stopNotifications":
				params, err := NotificationsParamsFromJson(msg.Params)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}
				log.Printf("stopNotifications params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					log.Errorf("get device characteristic error: %s\n", err)
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				err = char.EnableNotifications(nil)
				if err != nil {
					_ = WsSend(c, msg.Error(err.Error()))
					continue
				}

				_ = WsSend(c, msg.Respond(nil))

			default:
				log.Errorf("unknown command '%s' with params: %+v\n", msg.Method, msg.DebugParams())
			}
		}

	})
}
