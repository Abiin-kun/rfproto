# 01 — Process & IPC architecture

Source: string/symbol archaeology over shipped binaries (raw dumps in `/home/flame/Projects/ree/raw/`),
2026-10-15. Literals in backticks are observations; tag legend in `00-overview.md`.

## 1. Process topology

```
[SCM: ReadyForService.exe  (service; source dir DemonService\)]
   └─ CreateProcessAsUser → [user session] SmartConnect.exe   (CEF UI + winapi.dll host)
         ├─ winapi.dll plugins.Start(plugins.json)
         │     ├─ NATIVE(16) in-proc DLLs: AppStream, ClipBoard, DragDrop, VirtualCameraPlugin
         │     └─ spawned exes: ScreenSharePlugin.exe, VaultPlugin.exe, SmartAIPlugin.exe, iOSPlugin.exe
         ├─ RunScreenShareService → screenshare\ScreenShareService.exe
         └─ local HTTPS+WS server (127.0.0.1:<port>, default literal 19527)
                └─ Vue UI (html/assets/index-Dg90TNrz.js) over wss://…​/readyfor/…
[launcher.exe]        → starts or attaches SmartConnect.exe; net/rpc client on \\.\pipe\ScRpcNamedPipe
[ScMcpServer.exe]     → same attach/spawn; MCP bridge for external hosts (Lenovo Qira)
```

Evidence:
- Service lifecycle: `Service_Main`, `StartServiceCtrlDispatcherW`, args `install|remove|renameShortCut|installCloudyDriver|runReadyFor`; launches UI with `RunReadyFor WTSQueryUserToken hToken:%p` / `CreateProcessAsUser`; kills/restarts with `KillReadyFor %ul`, `ReStartReadyfor %d` [strings_ReadyForService.exe.txt].
- Service-side actions over its own pipe client (`DaemonNamedPipeClient.cpp`): `OpenFireWall`, `CloseFireWall`, `InstallCameraDriver`, `InstallMotoDriveDriver`, `InstallDokanDriver`, `InstallScreenShareDriver`, `RunScreenShareService`, `stop_for_update`, `hibernate`.
- Helpers spawn the app: `Start SmartConnect: %s`, `SmartConnect is running`, `NamedPipeServerProcessId:%d` [strings_launcher.exe.txt]; same `client.runSmartConnect` in ScMcpServer [gosym_ScMcpServer.exe.txt].
- Plugin host API in winapi.dll: `plugins.Start/Stop/startPlugin/startPluginIfNeed/waitPlugin/onPluginStateChanged` [gosym_winapi.dll.txt].
- `plugins.json` bootMode bitmask **confirmed**: `ENABLE(1)`, `BOOT_AUTO(2)`, `RERUN(4)`, `KILL(8)`, `NATIVE(16)` (from the file's own `bootNote` strings, e.g. `"ENABLE(1) + BOOT_AUTO(2) + NATIVE(16) = 19"`).

## 2. The four IPC layers

| # | Channel | Protocol | Participants |
|---|---|---|---|
| a | UI ↔ app | **WebSocket JSON** over local HTTPS (`wss://127.0.0.1:<port>/readyfor/...`, token `wt`) | Vue UI ↔ winapi webserver/websocket |
| b | helpers ↔ app | **Go net/rpc** (`readyforRpc.<Method>`, gob framing) over **named pipe** `\\.\pipe\ScRpcNamedPipe` | launcher, ScMcpServer, process plugins |
| c | in-proc native plugins | **C ABI** exported by winapi.dll (`ServiceManager_SendMessage/RecvMessage`, `Broadcast_*`, `GetServicePort`, `WebView_*`, `Webcam_*`, `DC_InsertEvent`, `FreeCString`) | AppStream, ClipBoard, DragDrop, VirtualCamera |
| d | process plugins | **string commands** `NEWPLUGIN_<NAME>_<CMD>` with JSON payload | ScreenShare/Vault/SmartAI/iOS/CoreApp |

### 2b. Named-pipe RPC
- Pipe: `\\.\pipe\ScRpcNamedPipe`; logs `CreateNamedPipe success`, `NamedPipeServerProcessId:%d`,
  `Read named pipe, ctx:%s`, `200 Connected to Go RPC` (also seen as `HTTP/1.0 200 Connected to Go RPC`).
- Go `net/rpc` confirmed: `net/rpc.(*Server).Register/RegisterName/suitableMethods/sendResponse`,
  `rpc: can't find method`, `rpc: can't find service`.
- Registered service name: **`readyforRpc`** (`readyforRpcSendMessage…`, `readyforRpc.` + `OnMessage %s`).
- Server side (winapi.dll): `framework/rpc/server.Serve, namedPipeLoop, startRpcNamePipe, Token, GetContext, sendModuleStateBroadcast`.
- Client API (RfaRpc.dll c-shared, from gosym): `Connect, ConnectWithCtx, Disconnect, rpcCall,
  GetServicePort, ForwardServicePort, ReverseServicePort, GetDeviceInfo, GetPairedDeviceInfo(List),
  GetWebsocketInfo, InsertEvent, IsSameDeviceId, SendMessage, RecvMessage, SendBroadcast, RecvBroadcast`.
- Pipe ACL/identity checks: `common/lcv.ValidateNamedPipeServer|ValidateNamedPipeClient|ValidateAssembly|ValidatePid`.
- RfaRpc.dll exposes C callback types to the host (`_Ctype_OnBroadcastFunc`) — the C++ host injects
  OnMessage/OnBroadcast function pointers.

### 3. Wire envelopes

**Device↔device websocket message** (Go struct json-tags in winapi.dll):
`SrcDeviceId "deviceId,omitempty"`, `DstDeviceId "-"`, `SrcServiceId "srcServiceId"`,
`DstServiceId "serviceId"`, `MsgId "msgId,omitempty"`, `Result "-"`, `LocalIp "-"`, `RemoteIp "-"`,
`Data "data,omitempty"`.
Native log mirrors it: `OnServiceMsg start deviceId=%s, serviceId=%d, srcServiceId=%d, localIp=%s, remoteIp=%s, datalength=%d data=%s`.

**UI websocket handshake** (first frame): `{remoteDeviceId:"localhost", serviceId:<SERVICE_ID_PC_WEBVIEW=100>, token, source:<pathname>}`;
normal frames `{ver, deviceId, serviceId, data}` with `wsToken`/`wsPathname`/`https` piggygybacked in `data`.

**UI endpoint**: `wss://${location.host}/readyfor/freerdp` (constant `_rn="/readyfor/freerdp"`); server paths
`/readyfor/rpc`, `/readyfor/rdp`, `/readyfor/freerdpHD`, `/readyfor/checkindex`, `/api/notifications/v1/ws`.
Token from `?wt=` or, in CEF, `window.cefQuery({request:"token"})`; validated by
`common/token.GenerateWebviewToken/VerifyWebviewToken`; failures log `token error`, `403 must local connect`.

**Plugin command JSON keys** (SmartConnectCoreApp): `deviceId`, `serviceId`, `srcServiceId`, `action`,
`broadcastId`, `msgId`, `result`, `localIp`. Additional tags seen in Go structs: `cap`, `rid`,
`senderId`, `remoteId`, `localId`, `wsPort`, `cmd`, `notify`, `tokenTimeout`, `bootMode`, `logLevel`,
`module`, `ctx`.

**Broadcast action names** (winapi.dll buckets): `service_state_changed`, `websocket_state_changed`,
`webserver_state_changed`, `pairing_state_changed`, `ble_scan_stopped_action`,
`ble_connection_state_changed_action`, `clipboard_synced_action`, `clipboard_local_updated_action`,
`network_state_changed_action`, `wallpaper_change_action`, `appstream_status_changed_action`,
`appstream_list_changed_action`, `window_state_changed`, `webview_ready_action`,
`ai_plugin_remote_all_changed`, `any_devices_connected` (plugins.json `bootBroadcasts`).

## 4. ServiceId / capability model

ServiceId enum recovered from the Vue bundle (enum `y0` in `index-Dg90TNrz.js`):

| ID | Name | Owner |
|---|---|---|
| 0 | MANAGER | — |
| 1 | QRCODE | pairing |
| 2 | FILE_EX | file explorer |
| 3 | IPCAM | VirtualCameraPlugin (`{"deviceId":"%s","serviceId":3,"state":%d,"isFromPc":%s}`) |
| 4 | PROXY | — |
| 5 | NTF | notifications |
| 6 / 7 | RDP_SERVER / RDP_CLIENT | MotoRDP/FreeRDP |
| 8 | WEBVIEW | UI channel |
| 9 | CLIPBOARD | ClipBoard.dll (`clipboard_synced_action`) |
| 10 | DISPLAY | — |
| 13 | HOTSPOT | NativeAPI Hotspot.cpp |
| 15 | APP | AppStream (app streaming) |
| 16 | INPUT | AppStream `Input\R4InputService.cpp` |
| 17 | APP_LIST | — |
| 19 | DEVICE_PAIRED | — |
| 20 | CLOUD_DRIVER | services/clouddriver |
| 25 | DEVICE_STATUS | — |
| 26 | UC | UniversalControl |
| 28 | ACCOUNT | services/account (Lenovo ID) |
| 29 | AI_TEST | — |
| 99 | PC_UI | — |
| 100 | PC_WEBVIEW | UI websocket handshake |
| 101–108 | WEB_QRCODE, WEB_DISPLAY_SETTINGS, CONTEXT_MENU, TUTORIAL, FILE_PAGE, WEBCAM_WEBVIEW, XT_CARD/NOTIFY, LOG | UI-side |
| 168 | FILES | VaultPlugin |
| 170 | ROOT_WIN | — |
| 201 | PLUGIN_FILE_STATION | VaultPlugin (file station) |
| 202 | PC_SCREENSHARE | ScreenSharePlugin |
| 204 | TO_IOS_PLUGIN | iOSPlugin |
| 205 | GALLERY | — |
| 303 | IOT_HERO_INTENT | winapi `services/Ai.on303ServiceMsg` |

`plugins.json` `caps` maps serviceId → capability flags (values `1` or `3`; semantics **(U)**),
`bootServiceIds` lists what a plugin serves, `bootBroadcasts` lists wake-up broadcasts.
SmartAIPlugin ids 311/313 = AI capability pluginIds (313 = device file search, per
`hasDeviceFileSearchCaps`); AI commands `searchcommon/searchpicture/searchfile/generateimage/catchmeup/summarize/transfer`.

### 4b. `plugins.json` verbatim, and the routing consequence (E31)

The shipped manifest is the authoritative plugin inventory:

| plugin | file | caps | bootServiceIds | bootMode |
|---|---|---|---|---|
| ScreenSharePlugin | `screenshare/ScreenSharePlugin.exe` | `{"202":1}` | `[202]` | 1 (ENABLE) |
| VaultPlugin | `vaultPlugin/VaultPlugin.exe` | `{"201":3,"168":3}` | `[201,168]` | 9 (ENABLE+KILL) |
| SmartAIPlugin | `SmartAIPlugin.exe` | `{"31":3,"311":3,"313":3}` | `[31,311,313]` | 5 (ENABLE+RERUN) |
| iOSPlugin | `iOSPlugin/iOSPlugin.exe` | `{"204":1}` | `[204]` | 5 |
| AppStream | `AppStream.dll` | `{"15":3,"16":3,"21":3}` | — | 17 (ENABLE+NATIVE) |
| ClipBoard | `ClipBoard.dll` | `{"9":3}` | — | 19 |
| DragDrop | `DragDrop.dll` | `{"24":3}` | — | 17 |
| Webcam | `VirtualCameraPlugin.dll` | `{"3":3}` | — | 17 |

`bootMode` bit 1 = ENABLE, bit 4 = RERUN, bit 8 = KILL, bit 16 = NATIVE (in-process). The NATIVE ones are
DLLs loaded into the host; the others are separate child processes.

**Routing consequence.** The host WS server routes inbound messages by `serviceId` to *every* registrant,
so a single id carries both phone traffic and host↔plugin RPC. That is why a lone-process prototype sees
plugin-addressed traffic (e.g. `svc 17`, `svc 168`) and why `serviceId` must be read as a **router key**,
not as "one feature between phone and PC". Worked example: file station is phone-side **168** but
host↔VaultPlugin **201** (E30/E31); screen share is **202** (`ScreenSharePlugin`), app streaming is
**15/16/21** (`AppStream`), clipboard is **9** (`ClipBoard`), webcam is **3**, drag-drop is **24**.

## 5. MCP (AI tool interface)

- **The MCP server lives inside winapi.dll** `services/McpServer`: `NewSmartConnectServer`,
  `AddOrUpdateTools`, `handleReadMcpServerResource`, `RefreshMcp`, `OpenQira`,
  `SetConnectDeviceFromMcpTool`, `SetStartScreenShareFromMcpTool`, `SetStartRdpFromMcpTool`,
  `SetOpenAppstreamFromMcpTool`; resource `json://mcpserver/list`.
- Tools exposed (embedded JSON): `smartconnect_connect_device`, `smartconnect_get_paired_devices`,
  `smartconnect_cross_control`, `smartconnect_webcam`, `smartconnect_pc_screen_share`,
  `smartconnect_mobile_screen_share`, `smartconnect_stream_mobile_application` (input: `deviceId`).
- Transports: streamable HTTP (`Failed to create streamable http transport`) and spawned stdio
  children from `\McpServers.json` (`startScStdio name:%s, remoteIp:%s, port:%d, cmd:%s`,
  `Quantum-McpMgmt-stdio`, registry `Software\Lenovo\Lenovo Qira\McpMgmt`).
- `ScMcpServer.exe` itself is only a **thin named-pipe bridge** for external MCP hosts
  (server name `smart-connect-mcp-server`, registry `SOFTWARE\Lenovo\ReadyFor`, `mcp.DeveloperConfig`) —
  its Go build has no mcp-go dependency.
- Gating: `framework/mcp.IsSCMcpEnabled/IsSCMcpON/IsQiraCapableMachine/SetQiraCapableMachine/UpdateMcpState`.

## 6. Inferences

- Ownership: the service owns the user-session app; the app owns plugins + ScreenShareService;
  launcher/ScMcpServer are peers that attach to the pipe and will spawn the app if absent.
- The C++ `SmartConnect.exe` is mostly a CEF shell + plugin loader; nearly all product logic is Go
  in `winapi.dll`/`RfaRpc.dll` ⇒ a Linux port can collapse this into **one Go daemon + a webview**.
- `ReadyForAssistant.exe` (source dir `tools\ReadyForDriverInstaller`) is the first-run driver
  installer only.

## 7. Unknowns

- Meaning of cap value `1` vs `3`; names for serviceIds 21 and 31.
- Exact codec on the pipe (gob vs custom JSON codec — `Context`/`Token` server types exist).
- SCM service display name literal (`DemonService` is only the source dir).
- smux usage details for `ReverseServicePort` (how streams are numbered/labels).
