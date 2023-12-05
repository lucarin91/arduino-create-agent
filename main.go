// Copyright 2022 Arduino SA
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Version 1.82
// Supports Windows, Linux, Mac, and Raspberry Pi, Beagle Bone Black

package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	cors "github.com/andela/gin-cors"
	cert "github.com/arduino/arduino-create-agent/certificates"
	"github.com/arduino/arduino-create-agent/config"
	"github.com/arduino/arduino-create-agent/globals"
	"github.com/arduino/arduino-create-agent/index"
	"github.com/arduino/arduino-create-agent/systray"
	"github.com/arduino/arduino-create-agent/tools"
	"github.com/arduino/arduino-create-agent/updater"
	v2 "github.com/arduino/arduino-create-agent/v2"
	paths "github.com/arduino/go-paths-helper"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/websocket"
	"tinygo.org/x/bluetooth"
	//"github.com/sanbornm/go-selfupdate/selfupdate" #included in update.go to change heavily
)

var (
	version = "x.x.x-dev" //don't modify it, Jenkins will take care
	commit  = "xxxxxxxx"  //don't modify it, Jenkins will take care
	port    string
	portSSL string
)

// regular flags
var (
	hibernate        = flag.Bool("hibernate", false, "start hibernated")
	genCert          = flag.Bool("generateCert", false, "")
	additionalConfig = flag.String("additional-config", "config.ini", "config file path")
	isLaunchSelf     = flag.Bool("ls", false, "launch self 5 seconds later")

	// Ignored flags for compatibility
	_ = flag.String("gc", "std", "Deprecated. Use the config.ini file")
	_ = flag.String("regex", "usb|acm|com", "Deprecated. Use the config.ini file")
)

// iniflags
var (
	address        = iniConf.String("address", "127.0.0.1", "The address where to listen. Defaults to localhost")
	appName        = iniConf.String("appName", "", "")
	gcType         = iniConf.String("gc", "std", "Type of garbage collection. std = Normal garbage collection allowing system to decide (this has been known to cause a stop the world in the middle of a CNC job which can cause lost responses from the CNC controller and thus stalled jobs. use max instead to solve.), off = let memory grow unbounded (you have to send in the gc command manually to garbage collect or you will run out of RAM eventually), max = Force garbage collection on each recv or send on a serial port (this minimizes stop the world events and thus lost serial responses, but increases CPU usage)")
	hostname       = iniConf.String("hostname", "unknown-hostname", "Override the hostname we get from the OS")
	httpProxy      = iniConf.String("httpProxy", "", "Proxy server for HTTP requests")
	httpsProxy     = iniConf.String("httpsProxy", "", "Proxy server for HTTPS requests")
	indexURL       = iniConf.String("indexURL", "https://downloads.arduino.cc/packages/package_staging_index.json", "The address from where to download the index json containing the location of upload tools")
	iniConf        = flag.NewFlagSet("ini", flag.ContinueOnError)
	logDump        = iniConf.String("log", "off", "off = (default)")
	origins        = iniConf.String("origins", "", "Allowed origin list for CORS")
	regExpFilter   = iniConf.String("regex", "usb|acm|com", "Regular expression to filter serial port list")
	signatureKey   = iniConf.String("signatureKey", globals.SignatureKey, "Pem-encoded public key to verify signed commandlines")
	updateURL      = iniConf.String("updateUrl", "", "")
	verbose        = iniConf.Bool("v", true, "show debug logging")
	crashreport    = iniConf.Bool("crashreport", false, "enable crashreport logging")
	autostartMacOS = iniConf.Bool("autostartMacOS", true, "the Arduino Create Agent is able to start automatically after login on macOS (launchd agent)")
)

var homeTemplate = template.Must(template.New("home").Parse(homeTemplateHTML))

// If you navigate to this server's homepage, you'll get this HTML
// so you can directly interact with the serial port server
//
//go:embed home.html
var homeTemplateHTML string

// global clients
var (
	Tools   tools.Tools
	Systray systray.Systray
	Index   *index.Resource
)

type logWriter struct{}

func (u *logWriter) Write(p []byte) (n int, err error) {
	h.broadcastSys <- p
	return len(p), nil
}

var loggerWs logWriter

func homeHandler(c *gin.Context) {
	homeTemplate.Execute(c.Writer, c.Request.Host)
}

func launchSelfLater() {
	log.Println("Going to launch myself 2 seconds later.")
	time.Sleep(2 * 1000 * time.Millisecond)
	log.Println("Done waiting 2 secs. Now launching...")
}

func main() {
	// prevents bad errors in OSX, such as '[NS...] is only safe to invoke on the main thread'.
	runtime.LockOSThread()

	// Parse regular flags
	flag.Parse()

	// Generate certificates
	if *genCert {
		cert.GenerateCertificates(config.GetCertificatesDir())
		os.Exit(0)
	}
	// Check if certificates made with Agent <=1.2.7 needs to be moved over the new location
	cert.MigrateCertificatesGeneratedWithOldAgentVersions(config.GetCertificatesDir())

	// Launch main loop in a goroutine
	go loop()

	// run ble servers
	go ble()

	// SetupSystray is the main thread
	configDir := config.GetDefaultConfigDir()
	Systray = systray.Systray{
		Hibernate: *hibernate,
		Version:   version + "-" + commit,
		DebugURL: func() string {
			return "http://" + *address + port
		},
		AdditionalConfig: *additionalConfig,
		ConfigDir:        configDir,
	}

	if src, err := os.Executable(); err != nil {
		panic(err)
	} else if restartPath := updater.Start(src); restartPath != "" {
		Systray.RestartWith(restartPath)
	} else {
		Systray.Start()
	}
}

var allowOriginFunc = func(r *http.Request) bool {
	return true
}

var MsgID int64 = 0

type Msg struct {
	Id      int64           `json:"id"`
	Jsonrpc string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func NewMsg(method string, params interface{}) Msg {
	buff, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	return Msg{
		Id:      atomic.AddInt64(&MsgID, 1),
		Jsonrpc: "2.0",
		Method:  method,
		Params:  json.RawMessage(buff),
	}
}

func (m Msg) RespondBytes(buf []byte) Result {
	return Result{
		Id:       m.Id,
		Jsonrpc:  "2.0",
		Encoding: "base64",
		Result:   base64.StdEncoding.EncodeToString(buf),
	}
}

func (m Msg) Respond(data interface{}) Result {
	return Result{
		Id:      m.Id,
		Jsonrpc: "2.0",
		Result:  data,
	}
}

func (m Msg) Error(err string) Error {
	return Error{
		Id:      m.Id,
		Jsonrpc: "2.0",
		Error:   err,
	}
}

func (m Msg) DebugParams() map[string]interface{} {
	var out map[string]interface{}
	err := json.Unmarshal(m.Params, &out)
	if err != nil {
		panic(err)
	}
	return out
}

type Result struct {
	Id       int64       `json:"id"`
	Jsonrpc  string      `json:"jsonrpc"`
	Result   interface{} `json:"result"`
	Encoding string      `json:"encoding,omitempty"`
}

type Error struct {
	Id      int64  `json:"id"`
	Jsonrpc string `json:"jsonrpc"`
	Error   string `json:"error"`
}

type Device struct {
	PeripheralId string `json:"peripheralId"`
	Name         string `json:"name"`
	RSSI         int16  `json:"rssi"`
}

type DiscoverParams struct {
	Filters []DiscoverFilter `json:"filters"`
}

type DiscoverFilter struct {
	Name       string      `json:"name"`
	NamePrefix string      `json:"namePrefix"`
	Services   []uuid.UUID `json:"services"`
}

type ConnectParams struct {
	PeripheralId string `json:"peripheralId"`
}

type NotificationsParams struct {
	ServiceId        uuid.UUID `json:"serviceId"`
	CharacteristicId uuid.UUID `json:"characteristicId"`
}

type UpdateParams struct {
	ServiceId        uuid.UUID `json:"serviceId"`
	CharacteristicId uuid.UUID `json:"characteristicId"`
	Message          string    `json:"message"`
	Encoding         string    `json:"encoding,omitempty"`
	WithResponse     bool      `json:"withResponse"`
}

type ReadParams struct {
	ServiceId          uuid.UUID `json:"serviceId"`
	CharacteristicId   uuid.UUID `json:"characteristicId"`
	StartNotifications bool      `json:"startNotifications"`
}

func WsSend(c *websocket.Conn, data interface{}) error {
	// fmt.Printf("[DEBUG] sending: %+v\n", data)
	buff, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}

	_, err = c.Write(buff)
	if err != nil {
		return fmt.Errorf("ws write error: %w", err)
	}

	return nil
}

func WsRead(c *websocket.Conn) (Msg, error) {
	buff := make([]byte, 512)
	var msg Msg
	for {
		n, err := c.Read(buff)
		if err == io.EOF {
			continue
		}
		if err != nil {
			return msg, fmt.Errorf("ws read error: %w", err)
		}
		if n >= 512 {
			panic("too big")
		}

		err = json.Unmarshal(buff[:n], &msg)
		if err != nil {
			return msg, fmt.Errorf("ws read error: %w", err)
		}
		if len(msg.Method) == 0 {
			// result message
			continue
		}

		return msg, nil
	}
}

func matchDevice(device bluetooth.ScanResult, filters []DiscoverFilter) bool {
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

func ble() {
	var adapter = bluetooth.DefaultAdapter

	if err := adapter.Enable(); err != nil {
		fmt.Printf("BLE not enabled: %s", err)
	}

	http.Handle("/scratch/ble", websocket.Handler(func(c *websocket.Conn) {
		fmt.Println("CONNECT")

		var DEVICE *bluetooth.Device

		for {
			msg, err := WsRead(c)
			if err != nil {
				fmt.Printf("err: %s\n", err)
				continue
			}

			switch msg.Method {
			case "getVersion":
				err := WsSend(c, msg.Respond(map[string]string{"protocol": "1.3"}))
				if err != nil {
					fmt.Printf("err: %s", err)
					continue
				}

			case "discover":
				var params DiscoverParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					fmt.Printf("err: %s\n", err)
					WsSend(c, msg.Error(err.Error()))
					continue
				}

				fmt.Println("scanning...")
				err = adapter.Scan(func(adapter *bluetooth.Adapter, device bluetooth.ScanResult) {
					if len(device.LocalName()) == 0 {
						return
					}

					println("found device:", device.Address.String(), device.RSSI, device.LocalName())

					if !matchDevice(device, params.Filters) {
						return
					}

					if err := adapter.StopScan(); err != nil {
						fmt.Printf("err: %s\n", err)
						return
					}

					msg := NewMsg("didDiscoverPeripheral", Device{
						PeripheralId: device.Address.String(),
						Name:         device.LocalName(),
						RSSI:         device.RSSI,
					})
					err := WsSend(c, msg)
					if err != nil {
						fmt.Printf("err: %s", err)
						return
					}
				})
				if err != nil {
					fmt.Printf("error: %s", err)
					WsSend(c, msg.Error(err.Error()))
					continue
				}

				err = WsSend(c, msg.Respond(nil))
				if err != nil {
					fmt.Printf("err: %s", err)
					continue
				}

			case "connect":
				var params ConnectParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					fmt.Printf("error: %s", err)
					WsSend(c, msg.Error(err.Error()))
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
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("error: %s", err)
					continue
				}
				fmt.Printf("device: %+v\n", *DEVICE)

				err = WsSend(c, msg.Respond(nil))
				if err != nil {
					fmt.Printf("err: %s", err)
					continue
				}

			case "startNotifications":
				var params NotificationsParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}
				fmt.Printf("startNotifications params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				err = char.EnableNotifications(notificationCallback(c, params.CharacteristicId, params.CharacteristicId))
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				err = WsSend(c, msg.Respond(nil))
					if err != nil {
					fmt.Printf("err: %s", err)
					continue
					}

			case "write":
				var params UpdateParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}
				fmt.Printf("write params: %+v\n", params)

				if params.Encoding != "base64" {
					panic("encoding format not supported")
				}

				services, err := DEVICE.DiscoverServices([]bluetooth.UUID{bluetooth.NewUUID(params.ServiceId)})
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{bluetooth.NewUUID(params.CharacteristicId)})
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}
				char := chars[0]

				buf, err := base64.StdEncoding.DecodeString(params.Message)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				// TODO: handle params.WithResponse
				n, err := char.WriteWithoutResponse(buf)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				err = WsSend(c, msg.Respond(n))
				if err != nil {
					fmt.Printf("err: %s\n", err)
					continue
				}

			case "read":
				var params ReadParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}
				fmt.Printf("read params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				if params.StartNotifications {
					err = char.EnableNotifications(notificationCallback(c, params.CharacteristicId, params.CharacteristicId))
					if err != nil {
						WsSend(c, msg.Error(err.Error()))
						fmt.Printf("err: %s\n", err)
						continue
					}
				}

				buf := make([]byte, 512)
				n, err := char.Read(buf)
				err = WsSend(c, msg.RespondBytes(buf[:n]))
				if err != nil {
					fmt.Printf("err: %s\n", err)
					continue
				}

			case "stopNotifications":
				var params NotificationsParams
				err := json.Unmarshal(msg.Params, &params)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}
				fmt.Printf("stopNotifications params: %+v\n", params)

				char, err := getDeviceCharacteristic(*DEVICE, bluetooth.NewUUID(params.ServiceId), bluetooth.NewUUID(params.CharacteristicId))
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					fmt.Printf("err: %s\n", err)
					continue
				}

				err = char.EnableNotifications(nil)
				if err != nil {
					WsSend(c, msg.Error(err.Error()))
					continue
				}

				err = WsSend(c, msg.Respond(nil))
				if err != nil {
					fmt.Printf("err: %s\n", err)
					continue
				}

			default:
				panic(fmt.Sprintf("unknown command '%s' with params: %+v\n", msg.Method, msg.DebugParams()))
			}
		}

	}))
	// err := http.ListenAndServeTLS(":20111", "server.crt", "server.key", nil)
	err := http.ListenAndServeTLS(":20110", "server.crt", "server.key", nil)
	if err != nil {
		panic("ListenAndServe: " + err.Error())
	}
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
		err := WsSend(c, NewMsg("characteristicDidChange", UpdateParams{
			ServiceId:        ServiceId,
			CharacteristicId: CharacteristicId,
			Message:          base64.StdEncoding.EncodeToString(buf),
			Encoding:         "base64",
		}))
		if err != nil {
			fmt.Printf("err: %s\n", err)
			return
		}
	}
}

func loop() {
	if *hibernate {
		return
	}

	log.SetLevel(log.InfoLevel)
	log.SetOutput(os.Stdout)

	// We used to install the agent in $HOME/Applications before versions <= 1.2.7-ventura
	// With version > 1.3.0 we changed the install path of the agent in /Applications.
	// If we are updating manually from 1.2.7 to 1.3.0 we have to uninstall the old agent manually first.
	// This check will inform the user if he needs to run the uninstall first
	if runtime.GOOS == "darwin" && oldInstallExists() {
		printDialog("Old agent installation of the Arduino Create Agent found, please uninstall it before launching the new one")
		os.Exit(0)
	}

	// Instantiate Index
	Index = index.Init(*indexURL, config.GetDataDir())

	logger := func(msg string) {
		mapD := map[string]string{"DownloadStatus": "Pending", "Msg": msg}
		mapB, _ := json.Marshal(mapD)
		h.broadcastSys <- mapB
	}

	// Instantiate Tools
	Tools = *tools.New(config.GetDataDir(), Index, logger)

	// Let's handle the config
	configDir := config.GetDefaultConfigDir()
	var configPath *paths.Path

	// see if the env var is defined, if it is take the config from there, this will override the default path
	if envConfig := os.Getenv("ARDUINO_CREATE_AGENT_CONFIG"); envConfig != "" {
		configPath = paths.New(envConfig)
		if configPath.NotExist() {
			log.Panicf("config from env var %s does not exists", envConfig)
		}
		log.Infof("using config from env variable: %s", configPath)
	} else if defaultConfigPath := configDir.Join("config.ini"); defaultConfigPath.Exist() {
		// by default take the config from the ~/.arduino-create/config.ini file
		configPath = defaultConfigPath
		log.Infof("using config from default: %s", configPath)
	} else {
		// Fall back to the old config.ini location
		src, _ := os.Executable()
		oldConfigPath := paths.New(src).Parent().Join("config.ini")
		if oldConfigPath.Exist() {
			err := oldConfigPath.CopyTo(defaultConfigPath)
			if err != nil {
				log.Errorf("cannot copy old %s, to %s, generating new config", oldConfigPath, configPath)
			} else {
				configPath = defaultConfigPath
				log.Infof("copied old %s, to %s", oldConfigPath, configPath)
			}
		}
	}
	if configPath == nil {
		configPath = config.GenerateConfig(configDir)
	}

	// Parse the config.ini
	args, err := parseIni(configPath.String())
	if err != nil {
		log.Panicf("config.ini cannot be parsed: %s", err)
	}
	err = iniConf.Parse(args)
	if err != nil {
		log.Panicf("cannot parse arguments: %s", err)
	}
	Systray.SetCurrentConfigFile(configPath)

	// Parse additional ini config if defined
	if len(*additionalConfig) > 0 {
		additionalConfigPath := paths.New(*additionalConfig)
		if additionalConfigPath.NotExist() {
			log.Infof("additional config file not found in %s", additionalConfigPath.String())
		} else {
			args, err = parseIni(additionalConfigPath.String())
			if err != nil {
				log.Panicf("additional config cannot be parsed: %s", err)
			}
			err = iniConf.Parse(args)
			if err != nil {
				log.Panicf("cannot parse arguments: %s", err)
			}
			log.Infof("using additional config from %s", additionalConfigPath.String())
		}
	}

	// see if we are supposed to wait 5 seconds
	if *isLaunchSelf {
		launchSelfLater()
	}

	log.Println("Version:" + version)

	// hostname
	hn, _ := os.Hostname()
	if *hostname == "unknown-hostname" {
		*hostname = hn
	}
	log.Println("Hostname:", *hostname)

	// turn off garbage collection
	// this is dangerous, as u could overflow memory
	//if *isGC {
	if *gcType == "std" {
		log.Println("Garbage collection is on using Standard mode, meaning we just let Golang determine when to garbage collect.")
	} else if *gcType == "max" {
		log.Println("Garbage collection is on for MAXIMUM real-time collecting on each send/recv from serial port. Higher CPU, but less stopping of the world to garbage collect since it is being done on a constant basis.")
	} else {
		log.Println("Garbage collection is off. Memory use will grow unbounded. You WILL RUN OUT OF RAM unless you send in the gc command to manually force garbage collection. Lower CPU, but progressive memory footprint.")
		debug.SetGCPercent(-1)
	}

	// If the httpProxy setting is set, use its value to override the
	// HTTP_PROXY environment variable. Setting this environment
	// variable ensures that all HTTP requests using net/http use this
	// proxy server.
	if *httpProxy != "" {
		log.Printf("Setting HTTP_PROXY variable to %v", *httpProxy)
		err := os.Setenv("HTTP_PROXY", *httpProxy)
		if err != nil {
			// The os.Setenv documentation doesn't specify how it can
			// fail, so I don't know how to handle this error
			// appropriately.
			panic(err)
		}
	}

	if *httpsProxy != "" {
		log.Printf("Setting HTTPS_PROXY variable to %v", *httpProxy)
		err := os.Setenv("HTTPS_PROXY", *httpProxy)
		if err != nil {
			// The os.Setenv documentation doesn't specify how it can
			// fail, so I don't know how to handle this error
			// appropriately.
			panic(err)
		}
	}

	// see if they provided a regex filter
	if len(*regExpFilter) > 0 {
		log.Printf("You specified a serial port regular expression filter: %v\n", *regExpFilter)
	}

	// list serial ports
	portList, _ := enumerateSerialPorts()
	log.Println("Your serial ports:")
	if len(portList) == 0 {
		log.Println("\tThere are no serial ports to list.")
	}
	for _, element := range portList {
		log.Printf("\t%v\n", element)

	}

	if !*verbose {
		log.Println("You can enter verbose mode to see all logging by setting the v key in the configuration file to true.")
		log.SetOutput(io.Discard)
	}

	// save crashreport to file
	if *crashreport {
		logFilename := "crashreport_" + time.Now().Format("20060102150405") + ".log"
		// handle logs directory creation
		logsDir := config.GetLogsDir()
		logFile, err := os.OpenFile(logsDir.Join(logFilename).String(), os.O_WRONLY|os.O_CREATE|os.O_SYNC|os.O_APPEND, 0644)
		if err != nil {
			log.Print("Cannot create file used for crash-report")
		} else {
			redirectStderr(logFile)
		}
	}

	// macos agent launchd autostart
	if runtime.GOOS == "darwin" {
		if *autostartMacOS {
			config.InstallPlistFile()
		} else {
			config.UninstallPlistFile()
		}
	}

	// launch the hub routine which is the singleton for the websocket server
	go h.run()
	// launch our serial port routine
	go sh.run()
	// launch our dummy data routine
	//go d.run()

	go discoverLoop()

	r := gin.New()

	socketHandler := wsHandler().ServeHTTP

	extraOrigins := []string{
		"https://create.arduino.cc",
		"https://cloud.arduino.cc",
		"https://app.arduino.cc",
	}

	for i := 8990; i < 9001; i++ {
		port := strconv.Itoa(i)
		extraOrigins = append(extraOrigins, "http://localhost:"+port)
		extraOrigins = append(extraOrigins, "https://localhost:"+port)
		extraOrigins = append(extraOrigins, "http://127.0.0.1:"+port)
		extraOrigins = append(extraOrigins, "https://127.0.0.1:"+port)
	}

	r.Use(cors.Middleware(cors.Config{
		Origins:         *origins + ", " + strings.Join(extraOrigins, ", "),
		Methods:         "GET, PUT, POST, DELETE",
		RequestHeaders:  "Origin, Authorization, Content-Type",
		ExposedHeaders:  "",
		MaxAge:          50 * time.Second,
		Credentials:     true,
		ValidateHeaders: false,
	}))

	r.LoadHTMLFiles("templates/nofirefox.html")

	r.GET("/", homeHandler)
	r.GET("/certificate.crt", cert.CertHandler)
	r.DELETE("/certificate.crt", cert.DeleteCertHandler)
	r.POST("/upload", uploadHandler)
	r.GET("/socket.io/", socketHandler)
	r.POST("/socket.io/", socketHandler)
	r.Handle("WS", "/socket.io/", socketHandler)
	r.Handle("WSS", "/socket.io/", socketHandler)
	r.GET("/info", infoHandler)
	r.POST("/killbrowser", killBrowserHandler)
	r.POST("/pause", pauseHandler)
	r.POST("/update", updateHandler)

	// Mount goa handlers
	goa := v2.Server(config.GetDataDir().String(), Index)
	r.Any("/v2/*path", gin.WrapH(goa))

	go func() {
		// check if certificates exist; if not, use plain http
		certsDir := config.GetCertificatesDir()
		if certsDir.Join("cert.pem").NotExist() {
			log.Error("Could not find HTTPS certificate. Using plain HTTP only.")
			return
		}

		start := 8990
		end := 9000
		i := start
		for i < end {
			i = i + 1
			portSSL = ":" + strconv.Itoa(i)
			if err := r.RunTLS(*address+portSSL, certsDir.Join("cert.pem").String(), certsDir.Join("key.pem").String()); err != nil {
				log.Printf("Error trying to bind to port: %v, so exiting...", err)
				continue
			} else {
				log.Print("Starting server and websocket (SSL) on " + *address + "" + port)
				break
			}
		}
	}()

	go func() {
		start := 8990
		end := 9000
		i := start
		for i < end {
			i = i + 1
			port = ":" + strconv.Itoa(i)
			if err := r.Run(*address + port); err != nil {
				log.Printf("Error trying to bind to port: %v, so exiting...", err)
				continue
			} else {
				log.Print("Starting server and websocket on " + *address + "" + port)
				break
			}
		}
	}()
}

// oldInstallExists will return true if an old installation of the agent exists (on macos) and is not the process running
func oldInstallExists() bool {
	oldAgentPath := config.GetDefaultHomeDir().Join("Applications", "ArduinoCreateAgent")
	currentBinary, _ := os.Executable()
	// if the current running binary is the old one we don't need to do anything
	binIsOld, _ := paths.New(currentBinary).IsInsideDir(oldAgentPath)
	if binIsOld {
		return false
	}
	return oldAgentPath.Join("ArduinoCreateAgent.app").Exist()
}

// printDialog will print a GUI error dialog on macos
func printDialog(dialogText string) {
	oscmd := exec.Command("osascript", "-e", "display dialog \""+dialogText+"\" buttons \"OK\" with title \"Error\"")
	_ = oscmd.Run()
}

func parseIni(filename string) (args []string, err error) {
	cfg, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: false, AllowPythonMultilineValues: true}, filename)
	if err != nil {
		return nil, err
	}

	for _, section := range cfg.Sections() {
		for key, val := range section.KeysHash() {
			// Ignore launchself
			if key == "ls" {
				continue
			} // Ignore configUpdateInterval
			if key == "configUpdateInterval" {
				continue
			} // Ignore name
			if key == "name" {
				continue
			}
			args = append(args, "-"+key+"="+val)
		}
	}

	return args, nil
}
