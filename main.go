// rfproto: prototype peer for Lenovo Ready For / Smart Connect discovery + pairing.
//
//	rfproto serve  [flags]   advertise _readyformdns._tcp + self-signed TLS/WS + emit a pairing QR
//	rfproto browse [secs]    dump every _readyformdns._tcp advert on the LAN (run against the real PC first)
//	rfproto ble    scan|adv  LE peripheral (service 0x5246 + chars 0x0726/0x0722) and LE scanner
//
// `serve -ble` also advertises over BLE through BlueZ, because the real PC always does
// (docs/05-route-a-crypto.md §12.3) and the phone expects that peer to exist.
//
// Wire-confirmed facts baked in (docs/05-route-a-crypto.md, raw/{qr_decoded,pcap_analysis}.txt):
//   - mDNS service is "_readyformdns._tcp" (NOT "_readyfor._tcp").
//   - QR = literal prefix "motorolardpconnection" + JSON{version,wsData{...,fp,token,wsPort,...},ips?}.
//   - Phone scans QR, pins our TLS cert against the QR `fp`, then dials our wsPort.
//   - The PC presents device.Device{type:4, os:6, scType:2, cap:0x28ba, caps/… maps} and a BLE
//     advert "[0x11][cap&0xff][deviceId]" (docs/05-route-a-crypto.md §12.3/§12.4).
//     `-cap` is one source of truth for the mDNS TXT, the WS reply and the advert byte.
//
// Goal: see whether the phone lists this peer (mDNS) and/or scans the QR and connects (pairing),
// capture the in-band messages it sends, and answer them with the two messages the real PC sends
// (the discovery reply on connect, then the pair-ack when the phone confirms pairing).
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"
)

// defaultService is the wire-confirmed mDNS service type the phone advertises
// and (by symmetry) browses. Override with -service to A/B against `_readyfor._tcp`
// (a second label that also exists in the binary).
const defaultService = "_readyformdns._tcp"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	runtime.LockOSThread()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		serve([]string{"-ble"})
		return
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, "usage: rfproto (GUI + BLE) | rfproto serve [flags] | rfproto ctl [flags] [command] | rfproto tui | rfproto peers [-dir PATH] {list|forget ID} | rfproto browse [secs] | rfproto ble {scan|adv} [flags]")
	case "serve":
		serve(os.Args[2:])
	case "peers":
		if err := peersCommand(os.Args[2:], os.Stdout); err != nil {
			log.Fatal(err)
		}
	case "browse":
		browse(os.Args[2:])
	case "ble":
		bleCmd(os.Args[2:])
	case "ctl":
		if err := ctlCommand(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			log.Fatal(err)
		}
	case "tui":
		if err := ctlCommand(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// ---------- browse ----------

func browse(args []string) {
	fs := flag.NewFlagSet("browse", flag.ExitOnError)
	service := fs.String("service", defaultService, "mDNS service type to browse")
	fs.Parse(args)
	secs := 10
	if fs.NArg() > 0 {
		if n, err := strconv.Atoi(fs.Arg(0)); err == nil {
			secs = n
		}
	}
	r, err := zeroconf.NewResolver(nil)
	if err != nil {
		log.Fatal(err)
	}
	entries := make(chan *zeroconf.ServiceEntry)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range entries {
			log.Printf("[browse] instance=%q host=%s port=%d v4=%v v6=%v", e.Instance, e.HostName, e.Port, e.AddrIPv4, e.AddrIPv6)
			for _, t := range e.Text {
				log.Printf("[browse]   txt %s", t)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	if err := r.Browse(ctx, *service, "local.", entries); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	<-done
	log.Printf("[browse] done")
}

// ---------- ble ----------

// bleCmd exposes the BlueZ BLE peripheral/scanner:
//
//	rfproto ble scan [secs] [-id X]   watch LE adverts (decode Ready For payloads)
//	rfproto ble adv  [-id X] [...]    advertise as a Ready For PC until Ctrl-C
func bleCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rfproto ble {scan [secs] [-id X] | adv [-id X] [-name N] [-cap N] [-gatt=false]}")
		os.Exit(2)
	}
	switch args[0] {
	case "scan":
		fs := flag.NewFlagSet("ble scan", flag.ExitOnError)
		id := fs.String("id", "", "deviceId (base64) to label matches against")
		secs := fs.Int("secs", 20, "seconds to scan")
		fs.Parse(args[1:])
		if fs.NArg() > 0 {
			if n, err := strconv.Atoi(fs.Arg(0)); err == nil {
				*secs = n
			}
		}
		if err := bleScan(*secs, *id); err != nil {
			log.Fatal(err)
		}
	case "adv":
		host, _ := os.Hostname()
		fs := flag.NewFlagSet("ble adv", flag.ExitOnError)
		dir := fs.String("dir", "", "state dir (default: $XDG_STATE_HOME/rfproto or ~/.local/state/rfproto)")
		id := fs.String("id", "", "deviceId override (base64)")
		name := fs.String("name", host, "deviceName / LocalName")
		capv := fs.Int("cap", pcCapability&0xff, "byte 1 of the payload (low byte of the PC cap 0x28ba)")
		gatt := fs.Bool("gatt", true, "register the GATT application too")
		uuid128 := fs.Bool("uuid128", false, "advertise the 128-bit service UUID form")
		hexPayload := fs.String("hex", "", "override the advert payload (hex)")
		localName := fs.Bool("localname", true, "include LocalName in the advertisement")
		fs.Parse(args[1:])

		resolvedDir, err := resolveStateDir(*dir)
		if err != nil {
			log.Fatal(err)
		}
		*dir = resolvedDir
		if err := os.MkdirAll(*dir, 0o700); err != nil {
			log.Fatal(err)
		}
		devID := deviceID(*dir, *id)
		var payload []byte
		if *hexPayload != "" {
			decoded, err := hex.DecodeString(strings.NewReplacer(" ", "", ":", "").Replace(*hexPayload))
			if err != nil || len(decoded) < 3 {
				log.Fatalf("bad -hex %q: %v", *hexPayload, err)
			}
			payload = decoded
		}
		bp, err := bleStart(bleConfig{
			DeviceID:  devID,
			Name:      *name,
			CapLow:    byte(*capv),
			Payload:   payload,
			WithGATT:  *gatt,
			LocalName: *localName,
			ShortUUID: !*uuid128,
		})
		if err != nil {
			log.Fatal(err)
		}
		defer bp.Stop()
		log.Printf("[ble] advertising as %s (%s); Ctrl-C to stop", devID, *name)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("[ble] shutting down")
	default:
		log.Fatalf("unknown ble command %q", args[0])
	}
}

// ---------- serve ----------

func serve(args []string) {
	host, _ := os.Hostname()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "", "state dir (default: $XDG_STATE_HOME/rfproto or ~/.local/state/rfproto)")
	port := fs.Int("port", 19527, "TLS/WebSocket listen port (advertised as the service port AND the QR wsPort)")
	probe := fs.Int("probe", 19527, "plain TCP reachability port; separate listener only if != -port")
	service := fs.String("service", defaultService, "mDNS service type to advertise")
	name := fs.String("name", host, "deviceName")
	instance := fs.String("instance", "", "mDNS instance name (default: deviceName)")
	sn := fs.String("sn", "", "device sn = device.Device+0x38 (the PC uses the Windows MachineGuid); default: /etc/machine-id re-formatted as a GUID")
	sku := fs.String("sku", "", "device Sku = device.Device+0x48 (the PC uses the SMBIOS product name); default: /sys/class/dmi/id/product_name")
	skuKey := fs.String("skukey", "Sku", `json key for the Sku field: "Sku" (the tag in winapi.dll), "sku" (the phone's own spelling), or "both"`)
	id := fs.String("id", "", "deviceId override (default: persisted random 16 bytes, base64)")
	typ := fs.Int("type", 4, "mDNS type = device.Device+0x30 (the real PC QR says type=4)")
	osv := fs.Int("os", 6, "mDNS os = device.Device+0x31 (the real PC QR says os=6 = Windows)")
	capv := fs.Int("cap", pcCapability, "mDNS cap = device.Device+0x58 (the PC's default is 0x28ba=10426); also used in the WS reply and as the BLE advert byte")
	appType := fs.Int("apptype", 0, "mDNS appType (placeholder)")
	useRSA := fs.Bool("rsa", false, "RSA-2048 cert instead of ECDSA P-256 (only used when generating)")
	qrPath := fs.String("qr", "", "write pairing QR PNG (default: state dir/pairing.png; explicit empty disables)")
	noQR := fs.Bool("noqr", false, "skip QR generation entirely")
	wfd := fs.String("wfd", "", "WiFi-Direct (wfd) name for QR policies (default: deviceName)")
	token := fs.String("token", "", "QR token override (default: random per run)")
	capDir := fs.String("capture", "", "opt-in raw frame capture directory (contains pairing secrets; empty = off)")
	replyMsg := fs.String("reply", "auto", `reply to the first message: "auto" (recovered PC discovery reply), "echo" (repeat verbatim), a raw JSON string, or "" (stay silent)`)
	replyToken := fs.String("replytoken", "OK", `data.token value in the "auto" reply (the real PC sends the literal "OK")`)
	replyEach := fs.Bool("replyeach", false, "reply to every message instead of only the first")
	pairAck := fs.String("pair", "auto", `answer the phone's pair-confirm (manager cmd=1) with the recovered PC pair-ack: "auto", "off", or a raw JSON string`)
	pairCmd := fs.Int("paircmd", 1, "data.cmd value in the pair-ack (the PC sends 1; 2 means 'pairing rejected/cap reached')")
	bleOn := fs.Bool("ble", false, "also advertise over BLE via BlueZ (this is what the phone's 'turn on bluetooth on both devices' check most likely wants)")
	bleGATT := fs.Bool("blegatt", true, "with -ble, also register the GATT application (service 0x5246 + chars 0x0722/0x0726)")
	bleCap := fs.Int("blecap", -1, "byte 1 of the BLE advert payload; default -1 = low byte of -cap (PC cap 0x28ba -> 0xba)")
	bleUUID128 := fs.Bool("bleuuid128", false, "advertise the service UUID as 128-bit instead of the 16-bit 0x5246 form")
	bleHex := fs.String("blehex", "", "override the BLE advert payload (hex); default = 11 <cap> <base64-decoded deviceId>")
	bleLocalName := fs.Bool("blelocalname", true, "include LocalName (deviceName) in the BLE advertisement")
	share := fs.String("share", "", `after the pair-ack, ask the phone to start screen share: "mirror", "extend", or "" / "off" (serviceId 202, recovered payload; docs/03 §10)`)
	shareDelay := fs.Duration("sharedelay", 0, "with -share, wait this long after the pair-ack before sending the 202 request (tests whether the phone's handler is ready only after its bring-up settles)")
	shareID := fs.String("shareid", "device", `with -share, which phone id to put in data.deviceid: "device" (the changeable envelope id) or "real" (realDeviceId)`)
	shareRepeat := fs.Int("sharerepeat", 1, "with -share, how many times to send the 202 request (>1 resends after -shareinterval)")
	shareInterval := fs.Duration("shareinterval", 3*time.Second, "with -sharerepeat>1, the gap between resends")
	advertise202 := fs.Bool("advertise202", false, "PROBE (unfaithful): add service 202 to the PC's advertised caps/serviceVersions, testing whether the phone needs the peer to declare a service before it will act on a command for it")
	statusReply := fs.Bool("statusreply", true, "answer the phone's DEVICE_STATUS (serviceId 25) bring-up with the PC's own status; the phone stores that cap map and gates every feature tile on it (disable with -statusreply=false)")
	viewOnline := fs.Bool("viewonline", false, "PROBE: send a PC_WEBVIEW (serviceId 100) frame after the pair-ack (probe.go; cmd guessed — -echosvc 100 is the stronger test)")
	viewDelay := fs.Duration("viewdelay", 3*time.Second, "with -viewonline, delay after the pair-ack before the service-100 frame")
	echoSvcs := fs.String("echosvc", "", "PROBE: comma-separated serviceIds whose inbound frames are replayed back verbatim (schema-free liveness test), e.g. 25,9,16,100,32")
	fsReply := fs.String("fsreply", "", `PROBE: answer the file-station handshake (serviceId 168 cmd 1001); "auto" = best-effort serverInfo from VaultPlugin's keys, or a raw inner-JSON string`)
	fsSvc := fs.Int("fssvc", svcFiles, "with -fsreply, the serviceId to answer on (168 = where the phone sends; 201 is the PC's PLUGIN_FILE_STATION sender)")
	fsCmd := fs.Int("fscmd", 1001, "with -fsreply, the cmd to answer")
	castFlag := fs.String("cast", "", `PROBE: control the phone's receiving-side Ready For Assist client over service 1001: "mirror", "desktop", "mobileui", "stop", or "off"; this does NOT export the phone's display (use the local -ui workspace)`)
	castW := fs.Int("castw", 1920, "with -cast, requested cast width")
	castH := fs.Int("casth", 1080, "with -cast, requested cast height")
	castFPSFlag := fs.Float64("castfps", 30, "with -cast, requested cast fps")
	castVer := fs.String("castver", "1.0", `with -cast, the "ver" field in WsData/FeaturesServiceRequest/CastPhoneReqData`)
	castSvc := fs.Int("castsvc", svcFeatures, "with -cast, the wire serviceId to send on (1001 = the phone's features service)")
	castOuter := fs.Int("castcmd", castCmdRequest, "with -cast, the FeaturesServiceRequest.cmd (11 = cast request)")
	castDelayFlag := fs.Duration("castdelay", 0, "with -cast, wait this long after the pair-ack before sending")
	castRep := fs.Int("castrepeat", 1, "with -cast, how many times to send")
	castGap := fs.Duration("castinterval", 3*time.Second, "with -castrepeat>1, the gap between sends")
	castDev := fs.String("castdevice", "device", `with -cast, which PC id goes in FeaturesServiceRequest.deviceId: "device" (the envelope id the phone knows) or "real"`)
	castDry := fs.Bool("castdry", false, "with -cast, print the exact frame and exit without starting the server")
	castListenFlag := fs.Int("castlisten", 15, "with -cast, the listener id we register (cmd 1000); the phone addresses its CastPhoneStateData replies to this serviceId")
	castRegFlag := fs.Bool("castregister", true, "with -cast, send the cmd-1000 listener registration before the cast request")
	appIconsFlag := fs.Bool("appicons", false, "PROBE: on the phone's service-17 app list, fetch its advertised HTTPS service (port+token+cert fingerprint) and try the icon paths (appicons.go)")
	appIconHost := fs.String("appiconhost", "", "with -appicons, override the host (default: the phone's ws peer address)")
	appIconOut := fs.String("appiconout", "", "with -appicons, output dir (default: state dir/appicons)")
	appIconMax := fs.Int("appiconmax", 3, "with -appicons, how many app icons to try")
	injectAddr := fs.String("inject", "", "loopback address for a live control socket, e.g. 127.0.0.1:19528 (POST /send with a raw frame body, GET /state)")
	ctlAddr := fs.String("ctl", "", "control API address (default: private state-directory UNIX socket; host:port or unix:/absolute/path; empty disables)")
	uiEnabled := fs.Bool("ui", true, "open the native SDL phone workspace (false for headless serving)")
	usbInterface := fs.String("usb-interface", "", "USB Ethernet interface for Smart Connect cable mode")
	usbPeer := fs.String("usb-peer", "", "phone's IPv4 address on the USB link (required with -usb-interface)")
	var ipList multiFlag
	fs.Var(&ipList, "ip", "LAN IP to advertise in QR ips/wifi (repeatable; default: auto-detect all non-loopback)")
	var sets multiFlag
	fs.Var(&sets, "set", "override/add TXT entry, e.g. -set type=2 (repeatable)")
	fs.Parse(args)
	usbMode := *usbInterface != ""
	listenHost := ""
	var cable usbLink
	if usbMode != (*usbPeer != "") {
		log.Fatal("-usb-interface and -usb-peer must be used together")
	}
	if usbMode {
		if len(ipList) != 0 || *bleOn || *service != defaultService {
			log.Fatal("USB cable mode cannot be combined with -ip, -ble, or a custom -service")
		}
		var err error
		cable, err = openUSBLink(*usbInterface, *usbPeer)
		if err != nil {
			log.Fatal(err)
		}
		listenHost = cable.addr
		ipList = multiFlag{cable.addr}
		*service = usbDiscoveryService
		*noQR = true
	}
	resolvedDir, err := resolveStateDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	*dir = resolvedDir
	qrExplicit := false
	ctlExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "qr" {
			qrExplicit = true
		}
		if f.Name == "ctl" {
			ctlExplicit = true
		}
	})
	if !ctlExplicit {
		*ctlAddr, err = defaultControlAddr(*dir)
		if err != nil {
			log.Fatal(err)
		}
	}
	if !qrExplicit {
		*qrPath = filepath.Join(*dir, "pairing.png")
	}
	if *appIconOut == "" {
		*appIconOut = filepath.Join(*dir, "appicons")
	}

	captureDir = *capDir
	if captureDir != "" {
		if err := os.MkdirAll(captureDir, 0o700); err != nil {
			log.Fatalf("capture dir: %v", err)
		}
		seedSessions(captureDir)
		log.Printf("[cap] WARNING: raw captures contain pairing secrets; protect this directory")
	}
	replyToFirst = *replyMsg
	replyEvery = *replyEach
	replyTokenValue = *replyToken
	pairAckMode = *pairAck
	pairAckCmd = *pairCmd
	shareOnPair = *share
	shareDelayAfterPair = *shareDelay
	shareIDMode = *shareID
	shareRepeatCount = *shareRepeat
	shareRepeatGap = *shareInterval
	if *advertise202 {
		advertise202Probe()
		log.Printf("[probe] -advertise202: caps[%d]=2 serviceVersions[%d]=2", svcPCScreenShare, svcPCScreenShare)
	}
	probeStatusReply = *statusReply
	probeViewOnline = *viewOnline
	probeEchoSvcs = parseSvcSet(*echoSvcs)
	probeFSReply = *fsReply
	probeFSSvc = *fsSvc
	probeFSCmd = *fsCmd
	probeViewDelay = *viewDelay
	castMode = *castFlag
	castWidth = *castW
	castHeight = *castH
	castFPS = *castFPSFlag
	castVersion = *castVer
	castServiceID = *castSvc
	castOuterCmd = *castOuter
	castDelay = *castDelayFlag
	castRepeat = *castRep
	castRepeatGap = *castGap
	castDeviceIDMode = *castDev
	castListen = *castListenFlag
	castRegister = *castRegFlag
	appIconsOn = *appIconsFlag
	appIconsHost = *appIconHost
	appIconsOut = *appIconOut
	appIconsMax = *appIconMax
	if castMode != "" && castMode != "off" {
		if _, ok := castModeCmd(castMode); !ok {
			log.Fatalf("-cast %q: want mirror, desktop, mobileui, stop, or off", castMode)
		}
		log.Printf("[cast] cast=%s svc=%d cmd=%d %dx%d@%g ver=%s delay=%s x%d",
			castMode, castServiceID, castOuterCmd, castWidth, castHeight, castFPS, castVersion, castDelay, castRepeat)
	}
	if appIconsOn {
		log.Printf("[appicons] enabled: host=%q out=%s max=%d", appIconsHost, appIconsOut, appIconsMax)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}
	store, err := openPeerStore(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.close()
	trustedAdults = store
	startInjectServer(*injectAddr)
	var assets *frontendObserver
	if *uiEnabled || *ctlAddr != "" {
		streams = newViewerHub()
		streams.mediaInterface = *usbInterface
		defer streams.close()
		assets = newFrontendObserver(*dir)
		frontendAssets = assets
		frontendObserverHook = assets.Observe
		defer func() { frontendObserverHook = nil; frontendAssets = nil }()
	}
	control, err := startControlServer(*ctlAddr, store)
	if err != nil {
		log.Fatalf("[ctl] %v", err)
	}
	defer control.Close()
	if probeStatusReply || probeViewOnline || probeFSReply != "" || len(probeEchoSvcs) > 0 {
		log.Printf("[probe] liveness probes: statusreply=%v viewonline=%v echosvc=%v fsreply=%q(svc=%d cmd=%d)",
			probeStatusReply, probeViewOnline, *echoSvcs, probeFSReply, probeFSSvc, probeFSCmd)
	}
	// -cap is device.Device+0x58: one source of truth for the mDNS TXT entry,
	// the WS discovery reply and (low byte) the BLE advert.
	deviceCapability = *capv

	cert, err := loadOrCreateCert(*dir, *useRSA, host)
	if err != nil {
		log.Fatal(err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	fp := base64.StdEncoding.EncodeToString(sum[:])
	log.Printf("cert sha256 hex=%s", hex.EncodeToString(sum[:]))
	log.Printf("cert fp (base64)=%s", fp)

	devID := deviceID(*dir, *id)
	inst := *instance
	if inst == "" {
		inst = *name
	}
	if usbMode {
		inst += "_usb"
	}
	txt := []string{
		"deviceId=" + devID,
		"deviceName=" + *name,
		fmt.Sprintf("type=%d", *typ),
		fmt.Sprintf("os=%d", *osv),
		fmt.Sprintf("cap=%d", *capv),
		fmt.Sprintf("timestamp=%d", time.Now().UnixMilli()),
		"fp=" + hex.EncodeToString(sum[:]),
		fmt.Sprintf("appType=%d", *appType),
	}
	txt = applyOverrides(txt, sets)

	// Our identity, used to build the "auto" pairing reply on connect.
	//
	// sn/sku are part of device.Device and the phone insists on them: it inserts
	// the whole object into `remote_device` and `sn` is NOT NULL there (the app
	// otherwise logs "insert RemoteDevice error! SQLiteConstraintException" and
	// then crashes). Defaults are this host's counterparts of the two Windows
	// registry values the PC uses (see identity.go); both are overridable.
	snValue, skuValue := *sn, *sku
	if snValue == "" {
		snValue = machineGUID(*dir)
		if snValue == "" {
			log.Fatal("[id] unable to generate a secure machine identity")
		}
	}
	if skuValue == "" {
		skuValue = machineProductName()
	}
	log.Printf("device sn=%s sku=%s (key %q)", snValue, skuValue, *skuKey)
	ourInfo = replyInfo{
		DeviceID: devID, Name: *name, FP: fp, Port: *port, Token: replyTokenValue,
		SN: snValue, SKU: skuValue, SKUKey: *skuKey,
	}
	ips := []string(ipList)
	if len(ips) == 0 {
		ips = lanIPList()
	}
	if len(ips) > 0 {
		ourInfo.IP = ips[0]
	}
	// Initialise authentication before accepting any connections.
	qrToken = *token
	if qrToken == "" {
		qrToken = newToken()
		if qrToken == "" {
			log.Fatal("[qr] secure random token generation failed")
		}
	}
	tok := qrToken
	wfdName := *wfd
	if wfdName == "" {
		wfdName = *name
	}
	var negWsData inviteWsData
	if usbMode {
		negWsData = newInviteWsData(devID, *name, fp, tok, nil, "", *port)
		negWsData.Policies = []map[string][]string{{"usb": ips}}
	} else {
		negWsData = newInviteWsData(devID, *name, fp, tok, ips, wfdName, *port)
	}
	reconnectEndpoint = negWsData

	if *castDry {
		if castMode == "" || castMode == "off" {
			log.Fatal("-castdry needs -cast mirror|desktop|mobileui|stop")
		}
		b, err := buildCastRequest(castMode, ourInfo.DeviceID)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(b))
		return
	}

	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.NoClientCert,
		NextProtos:   []string{"http/1.1"},
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			log.Printf("[tls] hello from %s sni=%q versions=%#x alpn=%v suites=%d",
				h.Conn.RemoteAddr(), h.ServerName, h.SupportedVersions, h.SupportedProtos, len(h.CipherSuites))
			return nil, nil
		},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				log.Printf("[tls] client sent NO certificate")
				return nil
			}
			s := sha256.Sum256(raw[0])
			log.Printf("[tls] client cert fp=%s", base64.StdEncoding.EncodeToString(s[:]))
			return nil
		},
	}

	listenAddr := net.JoinHostPort(listenHost, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening TLS on %s", ln.Addr())

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle)
	var handler http.Handler = mux
	if usbMode {
		mux.HandleFunc("/readyfor/rdp", usbDiscoveryHandler(negWsData))
		handler = cable.restrict(mux)
	}
	hs := &http.Server{
		Handler:  handler,
		ErrorLog: log.New(os.Stderr, "[srv] ", log.Ltime|log.Lmicroseconds),
	}
	go func() {
		if err := hs.Serve(tlsLogListener{Listener: ln, cfg: tc}); err != nil {
			log.Fatal(err)
		}
	}()

	var zs *zeroconf.Server
	if !usbMode {
		zs, err = zeroconf.Register(inst, *service, "local.", *port, txt, nil)
	} else {
		// Avoid sharing an mDNS hostname with the laptop's LAN addresses.
		usbHost := "rfproto-usb-" + hex.EncodeToString(sum[:6])
		zs, err = zeroconf.RegisterProxy(inst, *service, "local.", *port, usbHost, ips, txt, []net.Interface{cable.iface})
	}
	if err != nil {
		log.Fatalf("zeroconf register: %v", err)
	}
	defer zs.Shutdown()
	log.Printf("advertising %q %s.local. port=%d", inst, *service, *port)
	for _, t := range txt {
		log.Printf("  txt %s", t)
	}
	if *probe != *port {
		go probeListener(net.JoinHostPort(listenHost, strconv.Itoa(*probe)))
	}

	// Emit the pairing QR. The phone scans it, pins our cert against `fp`
	// (which we computed from the cert above, so the pin matches), and dials
	// our wsPort. This is the PC-initiated pairing path.
	//
	// The same endpoint data is used inside authenticated LAN/BLE replies.
	if !usbMode {
		negJSON, err := wsDataJSON(negWsData)
		if err != nil {
			log.Printf("[qr] wsData marshal: %v", err)
		} else {
			log.Printf("[qr] wsData (%dB, also the negotiation endpoint)", len(negJSON))
		}
	}
	if !*noQR {
		payload, err := buildInvite(devID, *name, fp, tok, ips, wfdName, *port)
		if err != nil {
			log.Printf("[qr] build: %v", err)
		} else {
			renderQR(payload, *qrPath)
			if assets != nil {
				assets.setPairingQR(payload)
			}
			log.Printf("[qr] scan this with the Ready For app; it should connect to :%d", *port)
		}
	}

	// Optional BLE peripheral. The real PC always advertises over BLE while the
	// adapter is on (framework/discovery/ble.onBleState -> startAdvertise); the
	// phone appears to require that peer to complete the pairing handshake.
	if *bleOn {
		var payload []byte
		if *bleHex != "" {
			decoded, err := hex.DecodeString(strings.NewReplacer(" ", "", ":", "").Replace(*bleHex))
			if err != nil || len(decoded) < 3 {
				log.Printf("[ble] bad -blehex %q: %v", *bleHex, err)
			} else {
				payload = decoded
			}
		}
		capLow := byte(deviceCapability & 0xff)
		if *bleCap >= 0 {
			capLow = byte(*bleCap)
		}
		bp, err := bleStart(bleConfig{
			DeviceID:  devID,
			Name:      *name,
			CapLow:    capLow,
			Payload:   payload,
			WithGATT:  *bleGATT,
			LocalName: *bleLocalName,
			ShortUUID: !*bleUUID128,
		})
		if err != nil {
			log.Printf("[ble] BLE advertising off: %v", err)
		} else {
			defer bp.Stop()
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	if *uiEnabled {
		if err := runNativeViewer(streams, assets, sig); err != nil {
			log.Fatalf("[ui] %v", err)
		}
	} else {
		<-sig
	}
	log.Printf("shutting down")
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// captureDir, when non-empty, receives every WebSocket frame the phone sends
// (raw bytes) plus a human-readable log, so nothing is lost to terminal truncation.
var captureDir string

// wsSessions counts WebSocket sessions so each one captures into its own
// subdirectory (the phone reconnects after pairing). It is seeded from the
// capture directory's existing c<N> dirs by seedSessions so that restarting
// serve does not reuse c1 and merge two runs' frames.
var wsSessions int64

// seedSessions makes the next session directory c<max+1> of any already present
// in dir. Without this the counter starts at 0 in every process, so a second
// run of `serve` writes into the same c1 as the first and the two sessions'
// frames (and their differently-sized filenames) end up interleaved.
func seedSessions(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var max int64
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(e.Name(), "c"), 10, 64)
		if err == nil && n > max {
			max = n
		}
	}
	if max > 0 {
		atomic.StoreInt64(&wsSessions, max)
		log.Printf("[cap] existing sessions up to c%d; next is c%d", max, max+1)
	}
}

// deviceCapability is device.Device+0x58 (json "cap"), set from -cap. The PC's
// own default is pcCapability (0x28ba, installed by device.init.0); it is shared
// by the mDNS TXT entry, the WS discovery/pair replies and the BLE advert byte.
var deviceCapability = pcCapability

// replyToFirst, when non-empty, is sent back on the first message we receive
// ("auto" builds the recovered PC discovery reply, "echo" repeats the received
// payload verbatim, anything else is sent as a raw JSON string). Used to probe
// the handshake.
var replyToFirst string

// replyEvery sends the reply on every received message instead of only the first.
var replyEvery bool

// replyTokenValue is the value placed in data.token of the "auto" reply.
var replyTokenValue string

// pairAckMode controls the response to the phone's pair-confirm (manager
// serviceId 0, data.cmd 1): "" / "off" to stay silent, "auto" to send the
// recovered PC pair-ack, anything else to send that raw JSON.
var pairAckMode string

// pairAckCmd is the data.cmd value put in the "auto" pair-ack.
var pairAckCmd int

// shareOnPair, when "mirror" or "extend", sends the recovered PC screen-share
// request (serviceId 202) right after the pair-ack. See screenshare.go and
// docs/03-screen-mirror.md §10.
var shareOnPair string

// shareDelayAfterPair delays the 202 send after the pair-ack (-sharedelay).
var shareDelayAfterPair time.Duration

// shareRepeatCount and shareRepeatGap resend the 202 request (-sharerepeat /
// -shareinterval); >1 guards against the phone not being ready on the first try.
var (
	shareRepeatCount = 1
	shareRepeatGap   = 3 * time.Second
)

// advertise202Probe adds service 202 to the PC's advertised caps and
// serviceVersions. It backs -advertise202 and is deliberately unfaithful: the
// recovered device.init.0 maps (pcCapabilities / pcServiceVersions) omit 202, so
// this only exists as a one-run probe for whether the phone requires the peer to
// declare a service before it will act on a command for that service.
func advertise202Probe() {
	pcCapabilities[svcPCScreenShare] = 2
	pcServiceVersions[svcPCScreenShare] = 2
}

// ourInfo is our own identity, used to build the "auto" reply.
var ourInfo replyInfo

// qrToken is the invite token we put in the QR, kept so we can tell whether the
// phone echoes it back in its first message (a strong hint about "verify").
var qrToken string

func handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyfor/mdns" {
		handleMDNS(w, r)
		return
	}
	if r.URL.Path != "/readyfor/" {
		http.NotFound(w, r)
		return
	}
	log.Printf("[http] %s %s %s from %s", r.Method, r.URL.RequestURI(), r.Proto, r.RemoteAddr)
	for k, v := range r.Header {
		log.Printf("[http]   %s: %s", k, strings.Join(v, ", "))
	}
	if !websocket.IsWebSocketUpgrade(r) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade failed: %v", err)
		return
	}
	defer c.Close()
	c.SetReadLimit(8 << 20)
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	c.SetCloseHandler(func(code int, text string) error {
		log.Printf("[ws] %s sent CLOSE code=%d reason=%q", r.RemoteAddr, code, text)
		return nil
	})
	log.Printf("[ws] %s connected", r.RemoteAddr)
	// Each WebSocket session keeps its own capture subdirectory: the phone
	// reconnects after pairing, and a flat naming scheme silently overwrites the
	// previous session's frames (which is exactly the evidence we need).
	sess := atomic.AddInt64(&wsSessions, 1)
	dir := captureDir
	if dir != "" {
		dir = filepath.Join(dir, fmt.Sprintf("c%d", sess))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("[cap] mkdir %s: %v", dir, err)
			dir = captureDir
		}
	}
	if dir != "" {
		log.Printf("[ws] session c%d: capturing frames to %s", sess, dir)
	}
	seq := 0
	shareSent := false
	castSent := false
	viewSent := false
	statusPushed := false
	peerID := ""
	peerRealID := ""
	var auth peerAuth
	ri := ourInfo
	probes := newProbeSession(peerHostOf(r.RemoteAddr))
	// WebSocket writes are not safe to interleave; the delayed/repeated screen-
	// share send runs in its own goroutine, so all writes go through one lock.
	var writeMu sync.Mutex
	sendFrame := func(kind string, out []byte) error {
		if out == nil {
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if err := writeFrame(c, kind, out); err != nil {
			_ = c.Close()
			return err
		}
		return nil
	}
	send := func(kind string, out []byte) { _ = sendFrame(kind, out) }
	// Make this session reachable through -inject so probes can be posted into
	// the already-open connection (no re-pair, no state reset).
	ls := &liveSession{seq: sess, peer: r.RemoteAddr, send: sendFrame}
	setLive(ls)
	defer clearLive(ls)
	for {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			log.Printf("[ws] %s closed: %v (session c%d ended after %d frame(s))", r.RemoteAddr, err, sess, seq)
			return
		}
		seq++
		if mt != websocket.TextMessage {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(3123, "verify failure"), time.Now().Add(time.Second))
			return
		}
		if seq == 1 {
			auth, err = authenticateHello(msg, qrToken, trustedAdults)
			if err != nil {
				log.Printf("[auth] %s rejected: %v", r.RemoteAddr, err)
				_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(3123, "verify failure"), time.Now().Add(time.Second))
				return
			}
			ri.Paired, ri.Key = auth.trusted, auth.key
			if auth.trusted {
				_ = c.SetReadDeadline(time.Time{})
			}
		} else if _, isPair := pairKeyFrom(msg); isPair {
			if pairAckMode != "auto" || pairAckCmd != 1 {
				if out, kind := pairAckFor(""); out != nil && pairAckMode != "" && pairAckMode != "off" {
					send(kind, out)
				}
				return
			}
			if _, err := auth.confirm(msg, trustedAdults); err != nil {
				log.Printf("[auth] pair-confirm rejected: %v", err)
				if out, err := buildPairAck(ri, "", 2); err == nil {
					send("pair-rejected", out)
				}
				return
			}
			ri.Paired, ri.Key = true, auth.key
			_ = c.SetReadDeadline(time.Time{})
		} else if !auth.trusted {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(3123, "verify failure"), time.Now().Add(time.Second))
			return
		}
		saveFrame(dir, seq, mt, msg)
		serviceID, _ := analyzeMsg(seq, msg)
		if peerID == "" {
			if id := peerDeviceID(msg); id != "" {
				peerID = id
				log.Printf("[ws] #%d phone deviceId=%s", seq, peerID)
			}
		}
		if peerRealID == "" {
			if id := peerRealDeviceID(msg); id != "" {
				peerRealID = id
				log.Printf("[ws] #%d phone realDeviceId=%s", seq, peerRealID)
			}
		}
		if replyToFirst != "" && (seq == 1 || replyEvery) {
			if out, kind := replyFor(msg, ri); out != nil {
				send(kind, out)
			}
		}
		if !auth.trusted {
			continue
		}
		probes.inbound(seq, msg, peerRealID, send)
		if pairAckMode != "" && pairAckMode != "off" {
			if key, ok := pairKeyFrom(msg); ok {
				log.Printf("[ws] #%d is the pair-confirm (manager cmd=1, key=%d B)", seq, len(key))
				if out, kind := pairAckFor(key); out != nil {
					send(kind, out)
				}
				// Probe: announce our PC_WEBVIEW state shortly after pairing.
				if !viewSent && probeViewOnline {
					viewSent = true
					go func() {
						time.Sleep(probeViewDelay)
						b, err := buildViewOnline()
						if err != nil {
							log.Printf("[probe] viewonline build: %v", err)
							return
						}
						log.Printf("[probe] viewonline -> serviceId %d (after %s)", svcPCWebView, probeViewDelay)
						send("viewonline", b)
					}()
				}
				// The recovered screen-share request is a follow-on to
				// pairing, so it goes out once, right after the pair-ack.
				if !shareSent {
					shareSent = true
					dev, real := peerID, peerRealID
					if shareOnPair != "" && shareOnPair != "off" {
						reps := shareRepeatCount
						if reps < 1 {
							reps = 1
						}
						target := shareTarget(dev, real)
						if target == "" {
							target = ourInfo.DeviceID
						}
						log.Printf("[share] %s -> data.deviceid=%s (shareid=%s, x%d, delay=%s)",
							shareOnPair, target, shareIDMode, reps, shareDelayAfterPair)
						go func() {
							for i := 0; i < reps; i++ {
								if shareDelayAfterPair > 0 {
									time.Sleep(shareDelayAfterPair)
								}
								b, kind := screenShareFor(dev, real)
								if b == nil {
									return
								}
								send(kind, b)
								if i < reps-1 {
									time.Sleep(shareRepeatGap)
								}
							}
						}()
					}
				}
				// Diagnostic service-1001 controls the phone's receiving-side
				// Ready For Assist client, not the phone-desktop producer.
				if !castSent && castMode != "" && castMode != "off" {
					castSent = true
					sendCast(peerRealID, send)
				}
				// The device page's feature tiles are gated on the phone's
				// service-25 cap cache (service/g.L -> g.q), which only an
				// inbound PC status frame fills. Answer the phone's own
				// status bring-up (probe.go) and push ours twice as well, in
				// case the phone's status listener registers after its first
				// frame already went by.
				if probeStatusReply && !statusPushed {
					statusPushed = true
					go func() {
						for _, d := range []time.Duration{2 * time.Second, 12 * time.Second} {
							time.Sleep(d)
							b, err := buildPCStatus()
							if err != nil {
								log.Printf("[probe] status push build: %v", err)
								return
							}
							log.Printf("[probe] status push -> serviceId %d (cap %d services)", svcDeviceStatus, len(pcCapabilities))
							send("status-push", b)
						}
					}()
				}
			}
		}
		observeFrontendFrame(serviceID, msg)
		if streams != nil {
			streams.inbound(ls, serviceID, msg)
		}
	}
}

// writeFrame sends one text frame and logs the outcome.
func writeFrame(c *websocket.Conn, kind string, out []byte) error {
	if err := c.WriteMessage(websocket.TextMessage, out); err != nil {
		log.Printf("[ws] %s write failed: %v", kind, err)
		return err
	}
	if kind == "auto" || kind == "echo" || kind == "raw" || strings.HasPrefix(kind, "pair") || strings.HasPrefix(kind, "stream") {
		log.Printf("[ws] replied (%s) %dB (credentials omitted)", kind, len(out))
	} else {
		log.Printf("[ws] replied (%s) %dB: %s", kind, len(out), clip(out, 2000))
	}
	return nil
}

// pairAckFor returns the frame to answer a pair-confirm with, per -pair.
func pairAckFor(key string) ([]byte, string) {
	if pairAckMode == "auto" {
		b, err := buildPairAck(ourInfo, key, pairAckCmd)
		if err != nil {
			log.Printf("[pair] build: %v", err)
			return nil, "pair(error)"
		}
		return b, "pair-ack"
	}
	return []byte(pairAckMode), "pair-raw"
}

// replyFor returns the frame to send for a received message, per -reply.
func replyFor(msg []byte, ri replyInfo) ([]byte, string) {
	switch replyToFirst {
	case "":
		return nil, "silent"
	case "echo":
		return msg, "echo"
	case "auto":
		b, err := buildDiscoveryReply(ri)
		if err != nil {
			log.Printf("[reply] build: %v", err)
			return nil, "auto(error)"
		}
		return b, "auto"
	default:
		return []byte(replyToFirst), "raw"
	}
}

// saveFrame writes a received frame to dir and logs a bounded preview.
func saveFrame(dir string, seq, mt int, msg []byte) {
	if dir != "" {
		base := filepath.Join(dir, fmt.Sprintf("%03d_op%d_%dB", seq, mt, len(msg)))
		if err := os.WriteFile(base+".bin", msg, 0o600); err != nil {
			log.Printf("[cap] write: %v", err)
		}
		note := base + ".txt"
		if json.Valid(msg) {
			os.WriteFile(note, msg, 0o600)
		} else {
			os.WriteFile(note, []byte(hex.Dump(msg)), 0o600)
		}
		log.Printf("[ws] recv #%d type=%d len=%d -> %s", seq, mt, len(msg), base)
	} else {
		log.Printf("[ws] recv #%d type=%d len=%d", seq, mt, len(msg))
	}
	// Manager, media signaling, catalog credentials, and input text stay private.
	var manager struct {
		ServiceID *int `json:"serviceId"`
	}
	if json.Unmarshal(msg, &manager) == nil && manager.ServiceID != nil &&
		(*manager.ServiceID == 0 || *manager.ServiceID == svcAppStream || *manager.ServiceID == svcInput ||
			*manager.ServiceID == svcAppList || *manager.ServiceID == svcPhoneAudio) {
		log.Printf("[ws] #%d service %d frame (private payload omitted)", seq, *manager.ServiceID)
		return
	}
	if json.Valid(msg) {
		log.Printf("[ws] #%d json: %s", seq, clip(msg, 4000))
	} else {
		log.Printf("[ws] #%d binary: %s", seq, dump(msg))
	}
}

func clip(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + fmt.Sprintf("...(%d more)", len(b)-max)
}

// analyzeMsg decodes the envelope of a received JSON frame and logs the fields
// we care about for the pairing handshake, notably whether the phone echoes the
// QR invite token (candidate "verify" material).
func analyzeMsg(seq int, msg []byte) (service int, sender int64) {
	var env struct {
		DeviceID     string `json:"deviceId"`
		MsgID        *int64 `json:"msgId"`
		SenderID     *int64 `json:"senderId"`
		ServiceID    *int64 `json:"serviceId"`
		SrcServiceID *int64 `json:"srcServiceId"`
		Data         struct {
			Cmd      *int64 `json:"cmd"`
			Paired   *int64 `json:"paired"`
			Token    string `json:"token"`
			ConnType *int64 `json:"connType"`
			IP       string `json:"ip"`
			Device   *struct {
				DeviceID     string `json:"deviceId"`
				RealDeviceID string `json:"realDeviceId"`
				DeviceName   string `json:"deviceName"`
				OS           *int64 `json:"os"`
				Type         *int64 `json:"type"`
				Cap          *int64 `json:"cap"`
			} `json:"device"`
			IPs []struct {
				ConnType *int64 `json:"connType"`
				IP       string `json:"ip"`
				Port     *int64 `json:"port"`
				FP       string `json:"fp"`
			} `json:"ips"`
		} `json:"data"`
	}
	if json.Unmarshal(msg, &env) != nil {
		return -1, 0
	}
	log.Printf("[msg] #%d envelope deviceId=%q msgId=%v senderId=%v serviceId=%v srcServiceId=%v",
		seq, env.DeviceID, deref(env.MsgID), deref(env.SenderID), deref(env.ServiceID), deref(env.SrcServiceID))
	log.Printf("[msg] #%d data cmd=%v paired=%v connType=%v ip=%s",
		seq, deref(env.Data.Cmd), deref(env.Data.Paired), deref(env.Data.ConnType), env.Data.IP)
	if d := env.Data.Device; d != nil {
		log.Printf("[msg] #%d data.device name=%q os=%v type=%v cap=%v deviceId=%q realDeviceId=%q",
			seq, d.DeviceName, deref(d.OS), deref(d.Type), deref(d.Cap), d.DeviceID, d.RealDeviceID)
	}
	for i, ip := range env.Data.IPs {
		log.Printf("[msg] #%d data.ips[%d] connType=%v %s:%v fp=%s",
			seq, i, deref(ip.ConnType), ip.IP, deref(ip.Port), ip.FP)
	}
	if env.Data.Token != "" {
		match := "NO QR YET"
		if qrToken != "" {
			if env.Data.Token == qrToken {
				match = "MATCHES our QR token"
			} else {
				match = "DIFFERS from our QR token"
			}
		}
		log.Printf("[msg] #%d invite token: %s", seq, match)
	}
	if env.ServiceID == nil {
		return -1, 0
	}
	if env.SenderID != nil {
		sender = *env.SenderID
	}
	return int(*env.ServiceID), sender
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func probeListener(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[probe] listen %s: %v", addr, err)
		return
	}
	log.Printf("[probe] plain TCP on %s", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		log.Printf("[probe] connect from %s", c.RemoteAddr())
		go func() {
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 256)
			if n, _ := c.Read(buf); n > 0 {
				log.Printf("[probe] %s sent %s", c.RemoteAddr(), dump(buf[:n]))
			}
		}()
	}
}

// ---------- helpers ----------

// dumpConn logs the first bytes read (post-TLS) so non-HTTP protocols are still visible.
type dumpConn struct {
	net.Conn
	left int
}

func (c *dumpConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.left > 0 {
		m := min(n, c.left)
		log.Printf("[raw %s] first bytes: %s", c.RemoteAddr(), dump(p[:m]))
		c.left -= m
	}
	return n, err
}

// tlsLogListener performs the TLS handshake inside Accept so a phone-side
// abort is logged with its reason (alert, EOF, version mismatch) instead of
// surfacing as a silent EOF on a half-open connection.
type tlsLogListener struct {
	net.Listener
	cfg *tls.Config
}

func (l tlsLogListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		t := tls.Server(c, l.cfg)
		if err := t.Handshake(); err != nil {
			log.Printf("[tls] handshake %s failed: %v", c.RemoteAddr(), err)
			c.Close()
			continue
		}
		_ = c.SetDeadline(time.Time{})
		st := t.ConnectionState()
		log.Printf("[tls] handshake %s ok version=%#x cipher=%#x alpn=%q", c.RemoteAddr(), st.Version, st.CipherSuite, st.NegotiatedProtocol)
		return t, nil
	}
}

type dumpListener struct{ net.Listener }

func (l dumpListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &dumpConn{Conn: c, left: 512}, nil
}

func dump(b []byte) string {
	const max = 256
	trunc := ""
	if len(b) > max {
		b, trunc = b[:max], "..."
	}
	for _, c := range b {
		if (c < 0x20 && c != '\n' && c != '\r' && c != '\t') || c > 0x7e {
			return "hex:" + hex.EncodeToString(b) + trunc
		}
	}
	return fmt.Sprintf("%q%s", b, trunc)
}

func applyOverrides(txt, sets []string) []string {
	for _, s := range sets {
		k, _, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		replaced := false
		for i, t := range txt {
			if strings.HasPrefix(t, k+"=") {
				txt[i], replaced = s, true
				break
			}
		}
		if !replaced {
			txt = append(txt, s)
		}
	}
	return txt
}

func deviceID(dir, override string) string {
	if override != "" {
		return override
	}
	p := filepath.Join(dir, "device_id")
	if b, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(b))
	}
	raw := make([]byte, 16) // real deviceIds are 16 bytes (24 base64 chars), e.g. "KIfv22hgTCCANmbV3jiOTg=="
	rand.Read(raw)
	id := base64.StdEncoding.EncodeToString(raw)
	os.WriteFile(p, []byte(id), 0o644)
	return id
}

func loadOrCreateCert(dir string, useRSA bool, host string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); err == nil {
		return tls.LoadX509KeyPair(certPath, keyPath)
	}
	var key crypto.Signer
	var err error
	if useRSA {
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}
