package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/jmoiron/sqlx" // Importação do sqlx
	"github.com/joho/godotenv"
	"github.com/mdp/qrterminal/v3"
	"github.com/patrickmn/go-cache"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

//var wlog waLog.Logger
var clientPointer = make(map[int]*whatsmeow.Client)
var clientHttp = make(map[int]*resty.Client)
var historySyncID int32

// Declaração do campo db como *sqlx.DB
type MyClient struct {
	WAClient       *whatsmeow.Client
	eventHandlerID uint32
	userID         int
	token          string
	subscriptions  []string
	db             *sqlx.DB
}

// Connects to Whatsapp Websocket on server startup if last state was connected
func (s *server) connectOnStartup() {
	rows, err := s.db.Queryx("SELECT id,token,jid,webhook,events FROM users WHERE connected=1")
	if err != nil {
		log.Error().Err(err).Msg("DB Problem")
		return
	}
	defer rows.Close()
	for rows.Next() {
		txtid := ""
		token := ""
		jid := ""
		webhook := ""
		events := ""
		err = rows.Scan(&txtid, &token, &jid, &webhook, &events)
		if err != nil {
			log.Error().Err(err).Msg("DB Problem")
			return
		} else {
			log.Info().Str("token", token).Msg("Connect to Whatsapp on startup")
			v := Values{map[string]string{
				"Id":      txtid,
				"Jid":     jid,
				"Webhook": webhook,
				"Token":   token,
				"Events":  events,
			}}
			userinfocache.Set(token, v, cache.NoExpiration)
			userid, _ := strconv.Atoi(txtid)
			// Gets and set subscription to webhook events
			eventarray := strings.Split(events, ",")

			var subscribedEvents []string
			if len(eventarray) < 1 {
				if !Find(subscribedEvents, "All") {
					subscribedEvents = append(subscribedEvents, "All")
				}
			} else {
				for _, arg := range eventarray {
					if !Find(messageTypes, arg) {
						log.Warn().Str("Type",arg).Msg("Message type discarded")
						continue
					}
					if !Find(subscribedEvents, arg) {
						subscribedEvents = append(subscribedEvents, arg)
					}
				}
			}
			eventstring := strings.Join(subscribedEvents, ",")
			log.Info().Str("events", eventstring).Str("jid",jid).Msg("Attempt to connect")
			killchannel[userid] = make(chan bool)
			go s.startClient(userid, jid, token, subscribedEvents)
		}
	}
	err = rows.Err()
	if err != nil {
		log.Error().Err(err).Msg("DB Problem")
	}
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
		    log.Error().Err(err).Msg("Invalid JID")
			return recipient, false
		} else if recipient.User == "" {
		    log.Error().Err(err).Msg("Invalid JID no server specified")
			return recipient, false
		}
		return recipient, true
	}
}

func (s *server) startClient(userID int, textjid string, token string, subscriptions []string) {

	log.Info().Str("userid", strconv.Itoa(userID)).Str("jid",textjid).Msg("Starting websocket connection to Whatsapp")

	var deviceStore *store.Device
	var err error

	if clientPointer[userID] != nil {
		isConnected := clientPointer[userID].IsConnected()
		if isConnected == true {
			return
		}
	}

	if textjid != "" {
		jid, _ := parseJID(textjid)
		// If you want multiple sessions, remember their JIDs and use .GetDevice(jid) or .GetAllDevices() instead.
		//deviceStore, err := container.GetFirstDevice()
		deviceStore, err = container.GetDevice(jid)
		if err != nil {
			panic(err)
		}
	} else {
		log.Warn().Msg("No jid found. Creating new device")
		deviceStore = container.NewDevice()
	}

	if deviceStore == nil {
		log.Warn().Msg("No store found. Creating new one")
		deviceStore = container.NewDevice()
	}

	//store.CompanionProps.PlatformType = waProto.CompanionProps_CHROME.Enum()
	//store.CompanionProps.Os = proto.String("Mac OS")

	osName := "Mac OS 10"
	store.DeviceProps.PlatformType = waProto.DeviceProps_UNKNOWN.Enum()
	store.DeviceProps.Os = &osName

	clientLog := waLog.Stdout("Client", *waDebug, *colorOutput)
	var client *whatsmeow.Client
	if(*waDebug!="") {
		client = whatsmeow.NewClient(deviceStore, clientLog)
	} else {
		client = whatsmeow.NewClient(deviceStore, nil)
	}
	clientPointer[userID] = client
	mycli := MyClient{client, 1, userID, token, subscriptions, s.db}
	mycli.eventHandlerID = mycli.WAClient.AddEventHandler(mycli.myEventHandler)

	//clientHttp[userID] = resty.New().EnableTrace()
	clientHttp[userID] = resty.New()
	clientHttp[userID].SetRedirectPolicy(resty.FlexibleRedirectPolicy(15))
	if *waDebug == "DEBUG" {
		clientHttp[userID].SetDebug(true)
	}
	clientHttp[userID].SetTimeout(30 * time.Second)
	clientHttp[userID].SetTLSClientConfig(&tls.Config{ InsecureSkipVerify: true })
	clientHttp[userID].OnError(func(req *resty.Request, err error) {
		if v, ok := err.(*resty.ResponseError); ok {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			log.Debug().Str("response",v.Response.String()).Msg("resty error")
			log.Error().Err(v.Err).Msg("resty error")
	  }
	})

	if client.Store.ID == nil {
		// No ID stored, new login

		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			// This error means that we're already logged in, so ignore it.
			if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
				log.Error().Err(err).Msg("Failed to get QR channel")
			}
		} else {
			err = client.Connect() // Si no conectamos no se puede generar QR
			if err != nil {
				panic(err)
			}
			for evt := range qrChan {
				if evt.Event == "code" {
					// Display QR code in terminal (useful for testing/developing)
					if(*logType!="json") {
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
						fmt.Println("QR code:\n", evt.Code)
						
					}
					// Store encoded/embeded base64 QR on database for retrieval with the /qr endpoint
					image, _ := qrcode.Encode(evt.Code, qrcode.Medium, 256)
					base64qrcode := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
					sqlStmt := `UPDATE users SET qrcode=$1 WHERE id=$2`
					_, err := s.db.Exec(sqlStmt, base64qrcode, userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					}

					// Adiciona envio do QR code por webhook
					postmap := make(map[string]interface{})
					postmap["type"] = "Connection.QRCode"
					postmap["event"] = map[string]interface{}{"code": evt.Code}

					jsonData, _ := json.Marshal(postmap)
					data := map[string]string{
						"jsonData": string(jsonData),
						"token":    token,
					}

					myuserinfo, found := userinfocache.Get(token)
					if found {
						webhookurl := myuserinfo.(Values).Get("Webhook")
						if webhookurl != "" {
							go callHook(webhookurl, data, userID)
						}
					}
				} else if evt.Event == "timeout" {
					// Clear QR code from DB on timeout
					sqlStmt := `UPDATE users SET qrcode=$1 WHERE id=$2`
					_, err := s.db.Exec(sqlStmt, "", userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					}
					log.Warn().Msg("QR timeout killing channel")

					// Adiciona dados para webhook
					postmap := make(map[string]interface{})
					postmap["type"] = "Connection.QRTimeout"
					postmap["event"] = map[string]interface{}{"code": evt.Code}
					jsonData, _ := json.Marshal(postmap)
					data := map[string]string{
						"jsonData": string(jsonData),
						"token":    token,
					}

					// Obtém webhook do usuário
					myuserinfo, found := userinfocache.Get(token)
					if found {
						webhookurl := myuserinfo.(Values).Get("Webhook")
						if webhookurl != "" {
							go callHook(webhookurl, data, userID)
						}
					}

					delete(clientPointer, userID)
					killchannel[userID] <- true
				} else if evt.Event == "success" {
					log.Info().Msg("QR pairing ok!")
					// Clear QR code after pairing
					sqlStmt := `UPDATE users SET qrcode=$1, connected=1 WHERE id=$2`
					_, err := s.db.Exec(sqlStmt, "", userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					}
				} else {
					log.Info().Str("event",evt.Event).Msg("Login event")
				}
			}
		}

	} else {
		// Already logged in, just connect
		log.Info().Msg("Already logged in, just connect")
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	// Keep connected client live until disconnected/killed
	for {
		select {
		case <-killchannel[userID]:
			log.Info().Str("userid",strconv.Itoa(userID)).Msg("Received kill signal")
			
			// Prepara e envia o webhook
			postmap := make(map[string]interface{})
			postmap["type"] = "Connection.LoggedOut"
			postmap["event"] = map[string]interface{}{"reason": "Kill signal received"}
			
			// Busca informações do usuário para o webhook
			myuserinfo, found := userinfocache.Get(token)
			if found {
				webhookurl := myuserinfo.(Values).Get("Webhook")
				if webhookurl != "" {
					jsonData, err := json.Marshal(postmap)
					if err == nil {
						data := map[string]string{
							"jsonData": string(jsonData),
							"token":    token,
						}
						go callHook(webhookurl, data, userID)
					}
				}
			}
			
			client.Disconnect()
			delete(clientPointer, userID)
			sqlStmt := `UPDATE users SET qrcode=$1, connected=0 WHERE id=$2`
			_, err := s.db.Exec(sqlStmt, "", userID)
			if err != nil {
				log.Error().Err(err).Msg(sqlStmt)
			}
			return
		default:
			time.Sleep(1000 * time.Millisecond)
			//log.Info().Str("jid",textjid).Msg("Loop the loop")
		}
	}
}

func fileToBase64(filepath string) (string, string, error) {
    data, err := os.ReadFile(filepath)
    if err != nil {
        return "", "", err
    }
    mimeType := http.DetectContentType(data)
    return base64.StdEncoding.EncodeToString(data), mimeType, nil
}

func (mycli *MyClient) myEventHandler(rawEvt interface{}) {
	// Carregar variáveis de ambiente
	err := godotenv.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Erro ao carregar o arquivo .env")
	}

	// Verificar se o logger de arquivo está habilitado
	enableLoggerFile := os.Getenv("ENABLE_LOGGER_FILE") == "true"

	// Abrir arquivo de log se habilitado
	var file *os.File
	if enableLoggerFile {
		file, err = os.OpenFile("event_log.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatal().Err(err).Msg("Erro ao abrir o arquivo de log")
		}
		defer file.Close()
	}

	// Função para registrar eventos no arquivo
	logEventToFile := func(event string) {
		if enableLoggerFile {
			if _, err := file.WriteString(fmt.Sprintf("%s - %s\n", time.Now().Format(time.RFC3339), event)); err != nil {
				log.Println("Erro ao escrever no arquivo de log:", err)
			}
		}
	}

	txtid := strconv.Itoa(mycli.userID)
	postmap := make(map[string]interface{})
	postmap["event"] = rawEvt
	dowebhook := 0
	path := ""

	ex, err := os.Executable()
	if err != nil {
		panic(err)
	}
	exPath := filepath.Dir(ex)

	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		logEventToFile("AppStateSyncComplete event")
		if len(mycli.WAClient.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := mycli.WAClient.SendPresence(types.PresenceAvailable)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to send available presence")
			} else {
				log.Info().Msg("Marked self as available")
			}
		}
	case *events.Connected, *events.PushNameSetting:
		logEventToFile("Connected or PushNameSetting event")
		if len(mycli.WAClient.Store.PushName) == 0 {
			return
		}
		// Send presence available when connecting and when the pushname is changed.
		// This makes sure that outgoing messages always have the right pushname.
		err := mycli.WAClient.SendPresence(types.PresenceAvailable)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to send available presence")
		} else {
			log.Info().Msg("Marked self as available")
		}
		sqlStmt := `UPDATE users SET connected=1 WHERE id=$1`
		_, err = mycli.db.Exec(sqlStmt, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
			return
		}
	case *events.PairSuccess:
		postmap["type"] = "Connection.PairSuccess"
		dowebhook = 1
		logEventToFile(fmt.Sprintf("PairSuccess event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("userid",strconv.Itoa(mycli.userID)).Str("token",mycli.token).Str("ID",evt.ID.String()).Str("BusinessName",evt.BusinessName).Str("Platform",evt.Platform).Msg("QR Pair Success")
		jid := evt.ID
		sqlStmt := `UPDATE users SET jid=$1 WHERE id=$2`
		_, err := mycli.db.Exec(sqlStmt, jid, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
			return
		}

		myuserinfo, found := userinfocache.Get(mycli.token)
		if !found {
			log.Warn().Msg("No user info cached on pairing?")
		} else {
			txtid := myuserinfo.(Values).Get("Id")
			token := myuserinfo.(Values).Get("Token")
			v := updateUserInfo(myuserinfo, "Jid", fmt.Sprintf("%s", jid))
			userinfocache.Set(token, v, cache.NoExpiration)
			log.Info().Str("jid",jid.String()).Str("userid",txtid).Str("token",token).Msg("User information set")
		}
	case *events.StreamReplaced:
		logEventToFile(fmt.Sprintf("StreamReplaced event: {type: %T, event: %+v}", evt, evt))
		log.Info().Msg("Received StreamReplaced event")
		return
	case *events.Message:
		logEventToFile(fmt.Sprintf("Message event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "Message"
		dowebhook = 1
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}

		log.Info().Str("id",evt.Info.ID).Str("source",evt.Info.SourceString()).Str("parts",strings.Join(metaParts,", ")).Msg("Message Received")
	


	case *events.Receipt:
		logEventToFile(fmt.Sprintf("Receipt event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "ReadReceipt"
		dowebhook = 1
		if evt.Type == events.ReceiptTypeRead || evt.Type == events.ReceiptTypeReadSelf {
			log.Info().Strs("id",evt.MessageIDs).Str("source",evt.SourceString()).Str("timestamp",fmt.Sprintf("%d",evt.Timestamp)).Msg("Message was read")
			if evt.Type == events.ReceiptTypeRead {
				postmap["state"] = "Read"
			} else {
				postmap["state"] = "ReadSelf"
			}
		} else if evt.Type == events.ReceiptTypeDelivered {
			postmap["state"] = "Delivered"
			log.Info().Str("id",evt.MessageIDs[0]).Str("source",evt.SourceString()).Str("timestamp",fmt.Sprintf("%d",evt.Timestamp)).Msg("Message delivered")
		} else {
			// Discard webhooks for inactive or other delivery types
			return
		}
	case *events.Presence:
		logEventToFile(fmt.Sprintf("Presence event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "Presence"
		dowebhook = 1
		if evt.Unavailable {
			postmap["state"] = "offline"
			if evt.LastSeen.IsZero() {
				log.Info().Str("from",evt.From.String()).Msg("User is now offline")
			} else {
				log.Info().Str("from",evt.From.String()).Str("lastSeen",fmt.Sprintf("%d",evt.LastSeen)).Msg("User is now offline")
			}
		} else {
			postmap["state"] = "online"
			log.Info().Str("from",evt.From.String()).Msg("User is now online")
		}
	case *events.HistorySync:
		logEventToFile(fmt.Sprintf("HistorySync event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "HistorySync"
		dowebhook = 1

		// check/creates user directory for files
		userDirectory := filepath.Join(exPath, "files", "user_"+txtid)
		_, err := os.Stat(userDirectory)
		if os.IsNotExist(err) {
			errDir := os.MkdirAll(userDirectory, 0751)
			if errDir != nil {
				log.Error().Err(errDir).Msg("Could not create user directory")
				return
			}
		}

		id := atomic.AddInt32(&historySyncID, 1)
		fileName := filepath.Join(userDirectory, "history-"+strconv.Itoa(int(id))+".json")
		file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			log.Error().Err(err).Msg("Failed to open file to write history sync")
			return
		}
		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		err = enc.Encode(evt.Data)
		if err != nil {
			log.Error().Err(err).Msg("Failed to write history sync")
			return
		}
		log.Info().Str("filename",fileName).Msg("Wrote history sync")
		_ = file.Close()
	case *events.AppState:
		logEventToFile(fmt.Sprintf("AppState event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("index",fmt.Sprintf("%+v",evt.Index)).Str("actionValue",fmt.Sprintf("%+v",evt.SyncActionValue)).Msg("App state event received")
	case *events.LoggedOut:
		logEventToFile(fmt.Sprintf("LoggedOut event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "Connection.LoggedOut"
		postmap["event"] = map[string]interface{}{"reason": evt.Reason.String()}
		dowebhook = 1
		log.Info().Str("reason",evt.Reason.String()).Msg("Logged out")
		killchannel[mycli.userID] <- true
		sqlStmt := `UPDATE users SET connected=0 WHERE id=$1`
		_, err := mycli.db.Exec(sqlStmt, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
			return
		}
	
	case *events.ChatPresence:
		logEventToFile(fmt.Sprintf("ChatPresence event: {type: %T, event: %+v}", evt, evt))
		postmap["type"] = "ChatPresence"
		dowebhook = 1
		log.Info().Str("state",fmt.Sprintf("%s",evt.State)).Str("media",fmt.Sprintf("%s",evt.Media)).Str("chat",evt.MessageSource.Chat.String()).Str("sender",evt.MessageSource.Sender.String()).Msg("Chat Presence received")
	case *events.CallOffer:
		logEventToFile(fmt.Sprintf("CallOffer event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got call offer")
	case *events.CallAccept:
		logEventToFile(fmt.Sprintf("CallAccept event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got call accept")
	case *events.CallTerminate:
		logEventToFile(fmt.Sprintf("CallTerminate event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got call terminate")
	case *events.CallOfferNotice:
		logEventToFile(fmt.Sprintf("CallOfferNotice event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got call offer notice")
	case *events.CallRelayLatency:
		logEventToFile(fmt.Sprintf("CallRelayLatency event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got call relay latency")

	case *events.QR:
		logEventToFile(fmt.Sprintf("QR event: {type: %T, event: %+v}", evt, evt))
		log.Info().Str("event",fmt.Sprintf("%+v",evt)).Msg("Got QR")
	default:
		logEventToFile(fmt.Sprintf("Unhandled event: {type: %T, event: %+v}", evt, evt))
		log.Warn().Str("event",fmt.Sprintf("%+v",evt)).Msg("Unhandled event")
	}

	if dowebhook == 1 {
		// call webhook
		webhookurl := ""
		myuserinfo, found := userinfocache.Get(mycli.token)
		if !found {
			log.Warn().Str("token",mycli.token).Msg("Could not call webhook as there is no user for this token")
		} else {
			webhookurl = myuserinfo.(Values).Get("Webhook")
		}

		if !Find(mycli.subscriptions, postmap["type"].(string)) && !Find(mycli.subscriptions, "All") {
			log.Warn().Str("type",postmap["type"].(string)).Msg("Skipping webhook. Not subscribed for this type")
			return
		}

if webhookurl != "" {
    log.Info().Str("url",webhookurl).Msg("Calling webhook")
    jsonData, err := json.Marshal(postmap)
    if err != nil {
        log.Error().Err(err).Msg("Failed to marshal postmap to JSON")
    } else {
        data := map[string]string{
            "jsonData": string(jsonData),
            "token":    mycli.token,
        }
        
        // Adicione este log
        log.Debug().Interface("webhookData", data).Msg("Data being sent to webhook")

        if path == "" {
            go callHook(webhookurl, data, mycli.userID)
        } else {
            // Create a channel to capture error from the goroutine
            errChan := make(chan error, 1)
            go func() {
                err := callHookFile(webhookurl, data, mycli.userID, path)
                errChan <- err
            }()
    
            // Optionally handle the error from the channel
            if err := <-errChan; err != nil {
                log.Error().Err(err).Msg("Error calling hook file")
            }
        }
    }
} else {
    log.Warn().Str("userid",strconv.Itoa(mycli.userID)).Msg("No webhook set for user")
}
	}
}

func logEventToFile(eventType string, evt interface{}) {
	// Formata o evento como JSON com indentação
	eventJSON, err := json.MarshalIndent(evt, "", "  ")
	if err != nil {
		log.Printf("Erro ao formatar evento: %v", err)
		return
	}

	// Registra o evento formatado com quebras de linha
	log.Println(fmt.Sprintf("%s:\n%s", eventType, string(eventJSON)))
}
