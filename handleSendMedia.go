package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/nfnt/resize"
	"github.com/vincent-petithory/dataurl"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func (s *server) SendMedia() http.HandlerFunc {
	type mediaRequest struct {
		MediaType     string              `json:"mediaType"` // "audio", "video", "image", "sticker", "document"
		Phone         string              `json:"phone"`
		Base64        string              `json:"base64,omitempty"`        // conteúdo base64, ex: "data:audio/ogg;base64,..."
		MediaUrl      string              `json:"mediaUrl,omitempty"`      // URL se quiser buscar do servidor remoto
		FileName      string              `json:"fileName,omitempty"`      // para documentos
		Caption       string              `json:"caption,omitempty"`       // imagem, vídeo ou documento
		JPEGThumbnail []byte              `json:"jpegThumbnail,omitempty"` // imagem / vídeo / sticker
		Id            string              `json:"id,omitempty"`
		ContextInfo   waProto.ContextInfo `json:"contextInfo"`
	}

	return func(w http.ResponseWriter, r *http.Request) {

		// Recupera o ID do usuário a partir do contexto
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		userid, _ := strconv.Atoi(txtid)

		// Checa se existe sessão para este usuário
		if clientPointer[userid] == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("Nenhuma sessão ativa para este usuário"))
			return
		}

		// Faz o parse do JSON de entrada
		decoder := json.NewDecoder(r.Body)
		var req mediaRequest
		if err := decoder.Decode(&req); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("Falha ao decodificar JSON de entrada"))
			return
		}

		// Valida entradas obrigatórias
		if req.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("Campo 'phone' é obrigatório"))
			return
		}
		if req.MediaType == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("Campo 'mediaType' é obrigatório"))
			return
		}

		// Monta e valida o destinatário
		recipient, err := validateMessageFields(req.Phone, req.ContextInfo.StanzaID, req.ContextInfo.Participant)
		if err != nil {
			log.Error().Msg(fmt.Sprintf("%s", err))
			s.Respond(w, r, http.StatusBadRequest, err)
			return
		}

		// ID da mensagem (se não vier, gera um)
		msgid := req.Id
		if msgid == "" {
			msgid = whatsmeow.GenerateMessageID()
		}

		// Identifica a mídia e faz o upload
		mediaType := strings.ToLower(req.MediaType)
		var uploaded whatsmeow.UploadResponse
		var filedata []byte

		// 1) Tenta decodificar do Base64, se foi fornecido
		// 2) Caso contrário, se "mediaUrl" estiver presente, faz o download
		if req.Base64 != "" {
			// Decodifica do base64
			fdat, mimeErr := decodeBase64(req.Base64)
			if mimeErr != nil {
				s.Respond(w, r, http.StatusBadRequest, mimeErr)
				return
			}
			filedata = fdat
		} else if req.MediaUrl != "" {
			// Busca via URL
			respData, fetchErr := fetchMediaFromUrl(req.MediaUrl)
			if fetchErr != nil {
				s.Respond(w, r, http.StatusBadRequest, fetchErr)
				return
			}
			filedata = respData

			// Se o fileName não existir, extraia do mediaUrl
			if req.FileName == "" && req.MediaUrl != "" {
				partesDaUrl := strings.Split(req.MediaUrl, "/")
				if len(partesDaUrl) > 0 {
					req.FileName = partesDaUrl[len(partesDaUrl)-1]
				}
			}
		} else {
			s.Respond(w, r, http.StatusBadRequest,
				errors.New("Você deve informar 'base64' ou 'mediaUrl'"))
			return
		}

		// Função auxiliar para setar ContextInfo (citação de mensagem anterior e menções)
		setContextInfo := func(msg *waProto.Message) {
			if req.ContextInfo.Expiration != nil {
				msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
					Expiration: proto.Uint32(*req.ContextInfo.Expiration),
				}
			}

			if req.ContextInfo.StanzaID != nil {
				if msg.ExtendedTextMessage == nil {
					msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{}
				}
				msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
					StanzaID:      proto.String(*req.ContextInfo.StanzaID),
					Participant:   proto.String(*req.ContextInfo.Participant),
					QuotedMessage: &waProto.Message{Conversation: proto.String("")},
				}
			}
			if req.ContextInfo.MentionedJID != nil {
				if msg.ExtendedTextMessage == nil {
					msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{}
				}
				if msg.ExtendedTextMessage.ContextInfo == nil {
					msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{}
				}
				msg.ExtendedTextMessage.ContextInfo.MentionedJID = req.ContextInfo.MentionedJID
			}
		}

		// Função de envio da mensagem
		sendMessage := func(msg *waProto.Message, logMsg string, erroMsg string) {
			resp, erro := clientPointer[userid].SendMessage(
				context.Background(), recipient, msg, whatsmeow.SendRequestExtra{ID: msgid},
			)
			if erro != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("%s: %v", erroMsg, erro))
				return
			}
			log.Info().Str("timestamp", fmt.Sprintf("%d", resp.Timestamp)).
				Str("id", msgid).
				Msg(logMsg)

			response := map[string]interface{}{"Details": logMsg, "Timestamp": resp.Timestamp, "Id": msgid}
			if rJSON, jsonErr := json.Marshal(response); jsonErr != nil {
				s.Respond(w, r, http.StatusInternalServerError, jsonErr)
			} else {
				s.Respond(w, r, http.StatusOK, string(rJSON))
			}
		}

		//-------------------------------------------------------------------
		// Seleciona a ação de acordo com mediaType
		//-------------------------------------------------------------------

		switch mediaType {

		case "audio":

			// Converte o arquivo WebM para OGG, se necessário
			var duration uint32
			if strings.HasSuffix(req.MediaUrl, ".webm") || strings.HasSuffix(req.MediaUrl, ".mp3") || strings.HasSuffix(req.MediaUrl, ".ogg") {

				filedata, duration, err = convertWebMToOgg(filedata, req.FileName)

				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao converter WebM para OGG: %v", err))
					return
				}
			}
			// Faz upload
			uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaAudio)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao fazer upload do áudio: %v", err))
				return
			}
			ptt := true
			mime := "audio/ogg; codecs=opus"
			msg := &waProto.Message{
				AudioMessage: &waProto.AudioMessage{
					URL:           proto.String(uploaded.URL),
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(mime),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(filedata))),
					PTT:           &ptt,
					Seconds:       proto.Uint32(uint32(duration)),
				},
			}
			setContextInfo(msg)

			sendMessage(msg, "Áudio enviado", "Erro ao enviar áudio")
			return

		case "video":
			var duration uint32
			filedata, duration, err = convertWebMToOgg(filedata, req.FileName)
			uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaVideo)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao fazer upload do vídeo: %v", err))
				return
			}
			mimetype := http.DetectContentType(filedata)
			msg := &waProto.Message{
				VideoMessage: &waProto.VideoMessage{
					Caption:       proto.String(req.Caption),
					URL:           proto.String(uploaded.URL),
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(mimetype),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(filedata))),
					JPEGThumbnail: req.JPEGThumbnail,
					Seconds:       proto.Uint32(uint32(duration)),
				},
			}
			setContextInfo(msg)
			sendMessage(msg, "Vídeo enviado", "Erro ao enviar vídeo")
			return

		case "image":
			uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaImage)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao fazer upload da imagem: %v", err))
				return
			}
			mime := http.DetectContentType(filedata)

			// Cria ou usa thumbnail
			thumb, _ := gerarThumbnailImagem(filedata)
			if len(thumb) == 0 && len(req.JPEGThumbnail) > 0 {
				thumb = req.JPEGThumbnail
			}

			msg := &waProto.Message{
				ImageMessage: &waProto.ImageMessage{
					Caption:       proto.String(req.Caption),
					URL:           proto.String(uploaded.URL),
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(mime),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(filedata))),
					JPEGThumbnail: thumb,
				},
			}
			if req.ContextInfo.StanzaID != nil {
				if msg.ImageMessage.ContextInfo == nil {
					msg.ImageMessage.ContextInfo = &waProto.ContextInfo{}
				}
				msg.ImageMessage.ContextInfo.StanzaID = proto.String(*req.ContextInfo.StanzaID)
				msg.ImageMessage.ContextInfo.Participant = proto.String(*req.ContextInfo.Participant)
				msg.ImageMessage.ContextInfo.QuotedMessage = &waProto.Message{Conversation: proto.String("")}
			}
			if req.ContextInfo.MentionedJID != nil {
				if msg.ImageMessage.ContextInfo == nil {
					msg.ImageMessage.ContextInfo = &waProto.ContextInfo{}
				}
				msg.ImageMessage.ContextInfo.MentionedJID = req.ContextInfo.MentionedJID
			}

			sendMessage(msg, "Imagem enviada", "Erro ao enviar imagem")
			return

		case "sticker":
			uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaImage)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao fazer upload do sticker: %v", err))
				return
			}
			mime := http.DetectContentType(filedata)

			msg := &waProto.Message{
				StickerMessage: &waProto.StickerMessage{
					URL:           proto.String(uploaded.URL),
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(mime),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(filedata))),
					PngThumbnail:  req.JPEGThumbnail,
				},
			}
			setContextInfo(msg)
			sendMessage(msg, "Sticker enviado", "Erro ao enviar sticker")
			return

		case "document":
			// Se não existir o fileName e for mediaUrl, use o nome do arquivo da URL
			if req.FileName == "" {
				if req.MediaUrl != "" {
					partesDaUrl := strings.Split(req.MediaUrl, "/")
					if len(partesDaUrl) > 0 {
						req.FileName = partesDaUrl[len(partesDaUrl)-1]
					}
				}

				// Se mesmo após a extração continuar vazio, retorna erro
				if req.FileName == "" {
					s.Respond(w, r, http.StatusBadRequest,
						errors.New("Para enviar documento, informe o 'fileName' ou inclua no final da URL"))
					return
				}
			}

			uploaded, err = clientPointer[userid].Upload(context.Background(), filedata, whatsmeow.MediaDocument)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("Falha ao fazer upload do documento: %v", err))
				return
			}
			mime := http.DetectContentType(filedata)
			msg := &waProto.Message{
				DocumentMessage: &waProto.DocumentMessage{
					URL:           proto.String(uploaded.URL),
					FileName:      &req.FileName,
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(mime),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(filedata))),
					Caption:       proto.String(req.Caption),
				},
			}
			setContextInfo(msg)
			sendMessage(msg, "Documento enviado", "Erro ao enviar documento")
			return

		default:
			s.Respond(w, r, http.StatusBadRequest, errors.New(
				"mediaType inválido (use 'audio', 'video', 'image', 'sticker' ou 'document')",
			))
			return
		}
	}
}

// gerarThumbnailImagem - gera automaticamente uma thumbnail 72x72 a partir de bytes de imagem.
// Ajuste conforme necessário, ou remova se preferir usar a thumbnail enviada diretamente pelo client.
func gerarThumbnailImagem(origem []byte) ([]byte, error) {
	reader := bytes.NewReader(origem)
	img, _, err := image.Decode(reader)
	if err != nil {
		return nil, err
	}
	// Redimensiona para 72x72 usando Lanczos3
	m := resize.Thumbnail(72, 72, img, resize.Lanczos3)

	tmpFile, err := os.CreateTemp("", "thumbnail-*.jpg")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if err := jpeg.Encode(tmpFile, m, nil); err != nil {
		return nil, err
	}
	if err := tmpFile.Sync(); err != nil {
		return nil, err
	}

	thumbBytes, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return nil, err
	}

	return thumbBytes, nil
}

// decodeBase64 - helper para decodificar string base64 que inicia com "data:..."
func decodeBase64(encoded string) ([]byte, error) {
	if !strings.HasPrefix(encoded, "data:") {
		return nil, errors.New("O campo 'base64' deve começar com 'data:...'")
	}
	dataURL, err := dataurl.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("Falha ao decodificar base64")
	}
	return dataURL.Data, nil
}

// fetchMediaFromUrl - busca o arquivo de uma URL, retornando os bytes.
func fetchMediaFromUrl(mediaUrl string) ([]byte, error) {
	resp, err := http.Get(mediaUrl)
	if err != nil {
		return nil, fmt.Errorf("Falha ao fazer GET na URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Falha ao buscar URL, status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Falha ao ler o conteúdo da URL: %v", err)
	}
	return body, nil
}

// convertWebMToOgg converte um arquivo WebM, MP3 ou OGG para o formato de áudio "audio/ogg; codecs=opus".
func convertWebMToOgg(input []byte, fileName string) ([]byte, uint32, error) {
	// Função auxiliar para imprimir detalhes do áudio usando ffprobe
	printAudioDetails := func(filePath string) (uint32, error) {

		cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", filePath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Erro ao obter detalhes do áudio com ffprobe: %v\n", err)
			return 0, err
		}

		details := strings.Split(strings.TrimSpace(string(output)), "\n")
		var duration uint32
		if len(details) >= 1 {
			fmt.Printf("Duration: %s\n", details[0])
			durationFloat, err := strconv.ParseFloat(details[0], 64)
			if err == nil {
				duration = uint32(math.Round(durationFloat))
			}
		} else {
			fmt.Println("Detalhes do áudio não foram encontrados ou estão incompletos.")
		}

		// Detecta o mimeType usando o comando file

		mimeCmd := exec.Command("file", "--mime-type", "-b", filePath)
		mimeOutput, err := mimeCmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Erro ao obter MIME Type: %v\n", err)
			return 0, err
		}
		fmt.Printf("MIME Type: %s\n", strings.TrimSpace(string(mimeOutput)))

		return duration, nil
	}

	// Cria um arquivo temporário para o arquivo de entrada

	inputFile, err := os.CreateTemp("", "input-*")
	if err != nil {
		fmt.Printf("Erro ao criar arquivo temporário de entrada: %v\n", err)
		return nil, 0, err
	}
	defer os.Remove(inputFile.Name())

	// Escreve os dados de entrada no arquivo temporário

	if _, err := inputFile.Write(input); err != nil {
		fmt.Printf("Erro ao escrever dados de entrada: %v\n", err)
		return nil, 0, err
	}
	inputFile.Close()

	// Imprime detalhes do áudio antes da conversão

	duration, err := printAudioDetails(inputFile.Name())
	if err != nil {
		return nil, 0, err
	}

	// Verifica se o arquivo precisa de conversão
	if strings.HasSuffix(fileName, ".webm") || strings.HasSuffix(fileName, ".mp3") {

		// Cria um arquivo temporário para o arquivo OGG de saída
		outputFile, err := os.CreateTemp("", "output-*.ogg")
		if err != nil {
			fmt.Printf("Erro ao criar arquivo temporário de saída: %v\n", err)
			return nil, 0, err
		}
		defer os.Remove(outputFile.Name())
		outputFile.Close()

		cmd := exec.Command("ffmpeg", "-y", "-i", inputFile.Name(), "-c:a", "libopus", "-b:a", "16k", "-ac", "1", "-ar", "48000", "-avoid_negative_ts", "make_zero", outputFile.Name())

		// Captura a saída de erro padrão do ffmpeg
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		// Executa o comando ffmpeg para converter o arquivo
		if err := cmd.Run(); err != nil {
			fmt.Printf("Erro ao executar ffmpeg: %v\n", err)
			fmt.Printf("Detalhes do erro do ffmpeg: %s\n", stderr.String())
			return nil, 0, err
		}

		duration, err = printAudioDetails(outputFile.Name())
		if err != nil {
			return nil, 0, err
		}

		convertedData, err := os.ReadFile(outputFile.Name())
		if err != nil {
			fmt.Printf("Erro ao ler arquivo de saída convertido: %v\n", err)
			return nil, 0, err
		}

		return convertedData, duration, nil
	}

	fmt.Println("Arquivo não requer conversão. Retornando dados originais.")
	// Se não for necessário converter, retorna os dados originais
	return input, duration, nil
}
