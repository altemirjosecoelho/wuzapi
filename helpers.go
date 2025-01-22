package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
)

func Find(slice []string, val string) bool {
    for _, item := range slice {
        if item == val {
            return true
        }
    }
    return false
}

// Update entry in User map
func updateUserInfo(values interface{}, field string, value string) interface{} {
    log.Debug().Str("field",field).Str("value",value).Msg("User info updated")
    values.(Values).m[field] = value
    return values
}

// webhook for regular messages
func callHook(myurl string, payload map[string]string, id int) {
    log.Info().Str("url",myurl).Msg("Sending POST to client "+strconv.Itoa(id))

    // Log the payload map
    log.Debug().Msg("Payload:")
    for key, value := range payload {
        log.Debug().Str(key, value).Msg("")
    }

    resp, err := clientHttp[id].R().SetFormData(payload).Post(myurl)
    if err != nil {
        log.Debug().Str("error",err.Error())
    }

    // Salvar log do webhook se estiver habilitado
    if os.Getenv("ENABLE_WEBHOOK_FILE") == "true" {
        logWebhook(myurl, payload, resp, err)
    }
}

// webhook for messages with file attachments
func callHookFile(myurl string, payload map[string]string, id int, file string) error {
    log.Info().Str("file", file).Str("url", myurl).Msg("Sending POST")

    // Criar um novo mapa para o payload final
    finalPayload := make(map[string]string)
    for k, v := range payload {
        finalPayload[k] = v
    }

    // Adicionar o arquivo ao payload
    finalPayload["file"] = file

    log.Debug().Interface("finalPayload", finalPayload).Msg("Final payload to be sent")

    resp, err := clientHttp[id].R().
        SetFiles(map[string]string{
            "file": file,
        }).
        SetFormData(finalPayload).
        Post(myurl)

    // Salvar log do webhook se estiver habilitado
    if os.Getenv("ENABLE_WEBHOOK_FILE") == "true" {
        logWebhook(myurl, finalPayload, resp, err)
    }

    if err != nil {
        log.Error().Err(err).Str("url", myurl).Msg("Failed to send POST request")
        return fmt.Errorf("failed to send POST request: %w", err)
    }

    // Log do payload enviado
    log.Debug().Interface("payload", finalPayload).Msg("Payload sent to webhook")

    // Optionally, you can log the response status and body
    log.Info().Int("status", resp.StatusCode()).Str("body", string(resp.Body())).Msg("POST request completed")

    return nil
}

// Função para salvar os logs do webhook em arquivo
func logWebhook(url string, payload map[string]string, resp *resty.Response, err error) {
    now := time.Now().Format("2006-01-02 15:04:05")
    logEntry := fmt.Sprintf("\n[%s] Webhook enviado\nURL: %s\nPayload: %v\n", now, url, payload)
    
    if err != nil {
        logEntry += fmt.Sprintf("Erro: %v\n", err)
    } else if resp != nil {
        logEntry += fmt.Sprintf("Status: %d\nResposta: %s\n", resp.StatusCode(), string(resp.Body()))
    }
    
    logEntry += "----------------------------------------\n"

    f, err := os.OpenFile("webhook_log.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        log.Error().Err(err).Msg("Erro ao abrir arquivo de log do webhook")
        return
    }
    defer f.Close()

    if _, err := f.WriteString(logEntry); err != nil {
        log.Error().Err(err).Msg("Erro ao escrever no arquivo de log do webhook")
    }
}