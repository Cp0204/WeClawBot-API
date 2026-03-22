package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"os/signal"
	"syscall"

	"github.com/mdp/qrterminal/v3"
)

const DefaultBaseURL = "https://ilinkai.weixin.qq.com"

type UserConfig struct {
	BotToken      string `json:"bot_token"`
	BotID         string `json:"bot_id"`
	GetUpdatesBuf string `json:"get_updates_buf"`
	IlinkUserID   string `json:"ilink_user_id"`
	ContextToken  string `json:"context_token"`
	APIToken      string `json:"api_token"`
}

type AppConfig struct {
	Bots map[string]*UserConfig `json:"bots"`
}

var (
	configPath = "./config/auth.json"
	cfg        AppConfig
	configLock sync.Mutex
	activeUser string // 当前控制台正在使用的 BotID
)

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func main() {
	port := flag.Int("port", 26322, "API server port")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Init config dir failed: %v", err)
	}

	loadConfig()

	if len(cfg.Bots) == 0 {
		fmt.Println("No login bots found. Starting QR Code login...")
		if err := doQRLogin(); err != nil {
			log.Printf("QR login failed: %v\n", err)
		}
	} else {
		fmt.Printf("Loaded %d bots.\n", len(cfg.Bots))
	}

	configLock.Lock()
	if len(cfg.Bots) == 1 {
		for botID := range cfg.Bots {
			activeUser = botID
			fmt.Printf("Auto selected single bot: %s\n", botID)
		}
	}

	// 为已存在但缺 token 的用户补齐 APIToken
	for _, user := range cfg.Bots {
		if user.APIToken == "" {
			user.APIToken = generateToken(16)
		}
	}
	configLock.Unlock()
	saveConfig()

	// 监听退出信号，安全退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nReceived shutdown signal. Saving config and exiting...")
		saveConfig()
		os.Exit(0)
	}()

	// 启动所有已有账号的监听协程
	for _, userCfg := range cfg.Bots {
		go monitorWeixin(userCfg)
	}

	go startAPIServer(*port)

	consoleReader()

	// 保持主程序运行（例如在 Docker 后台运行且没有 TTY 时）
	fmt.Println("Console closed or not available. Running in background...")
	select {}
}

func sendJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func getReqParam(r *http.Request, key string, jsonBody map[string]interface{}) string {
	if val, ok := jsonBody[key]; ok {
		return fmt.Sprint(val)
	}
	return r.FormValue(key)
}

func startAPIServer(port int) {
	http.HandleFunc("/bots/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bots/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Not Found"})
			return
		}

		botID := parts[0]
		action := parts[1]

		// 解析参数：支持 JSON, Multipart, Form, Query
		jsonBody := make(map[string]interface{})
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "application/json") {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &jsonBody)
		} else if strings.Contains(ct, "multipart/form-data") {
			r.ParseMultipartForm(10 << 20)
		} else {
			r.ParseForm()
		}

		token := ""
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			token = getReqParam(r, "token", jsonBody)
		}

		configLock.Lock()
		user, exists := cfg.Bots[botID]
		configLock.Unlock()

		if !exists {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Bot not found"})
			return
		}
		if user.APIToken != token || token == "" {
			sendJSON(w, http.StatusUnauthorized, map[string]interface{}{"code": 401, "error": "Unauthorized"})
			return
		}

		switch action {
		case "messages":
			text := getReqParam(r, "text", jsonBody)
			if text == "" {
				sendJSON(w, http.StatusBadRequest, map[string]interface{}{"code": 400, "error": "Missing text"})
				return
			}
			if user.IlinkUserID == "" || user.ContextToken == "" {
				sendJSON(w, http.StatusBadRequest, map[string]interface{}{"code": 400, "error": "Context not ready"})
				return
			}
			if err := sendMessage(user, user.IlinkUserID, text, user.ContextToken); err != nil {
				sendJSON(w, http.StatusInternalServerError, map[string]interface{}{"code": 500, "error": err.Error()})
			} else {
				sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "message": "OK"})
			}

		case "typing":
			statusStr := getReqParam(r, "status", jsonBody)
			status, _ := strconv.Atoi(statusStr)
			if status == 0 {
				status = 1 // Default to typing
			}
			if err := sendTypingWeixin(user, status); err != nil {
				sendJSON(w, http.StatusInternalServerError, map[string]interface{}{"code": 500, "error": err.Error()})
			} else {
				sendJSON(w, http.StatusOK, map[string]interface{}{"code": 200, "message": "OK"})
			}
		default:
			sendJSON(w, http.StatusNotFound, map[string]interface{}{"code": 404, "error": "Unknown action"})
		}
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("API Server listening on http://0.0.0.0%s\n", addr)
	http.ListenAndServe(addr, nil)
}

func getBotConfig(user *UserConfig) (string, error) {
	reqData := map[string]interface{}{
		"ilink_user_id": user.IlinkUserID,
		"context_token": user.ContextToken,
		"base_info": map[string]string{
			"channel_version": "1.0.0",
		},
	}
	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/getconfig", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var res struct {
		Ret          int    `json:"ret"`
		TypingTicket string `json:"typing_ticket"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Ret != 0 {
		return "", fmt.Errorf("getconfig ret %d", res.Ret)
	}
	return res.TypingTicket, nil
}

func sendTypingWeixin(user *UserConfig, status int) error {
	ticket, err := getBotConfig(user)
	if err != nil {
		return err
	}

	reqData := map[string]interface{}{
		"ilink_user_id": user.IlinkUserID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info": map[string]string{
			"channel_version": "1.0.0",
		},
	}
	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/sendtyping", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var res struct {
		Ret int `json:"ret"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Ret != 0 {
		return fmt.Errorf("sendtyping ret %d", res.Ret)
	}
	return nil
}

func loadConfig() {
	configLock.Lock()
	defer configLock.Unlock()
	data, err := os.ReadFile(configPath)
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.Bots == nil {
		cfg.Bots = make(map[string]*UserConfig)
	}
}

func saveConfig() {
	configLock.Lock()
	defer configLock.Unlock()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath, data, 0644)
}

func randomWechatUin() string {
	b := make([]byte, 4)
	rand.Read(b)
	val := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(val), 10)))
}

func commonHeaders(req *http.Request, isJson bool, token string) {
	if isJson {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomWechatUin())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func doQRLogin() error {
outer:
	for {
		resp, err := http.Get(DefaultBaseURL + "/ilink/bot/get_bot_qrcode?bot_type=3")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var qrRes struct {
			QRcode           string `json:"qrcode"`
			QRcodeImgContent string `json:"qrcode_img_content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&qrRes); err != nil {
			return err
		}

		qrterminal.GenerateHalfBlock(qrRes.QRcodeImgContent, qrterminal.L, os.Stdout)
		fmt.Println("Please scan the QR code to log in")

		for {
			statusReq, _ := http.NewRequest("GET", DefaultBaseURL+"/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(qrRes.QRcode), nil)
			commonHeaders(statusReq, false, "")
			statusReq.Header.Set("iLink-App-ClientVersion", "1")

			client := &http.Client{Timeout: 35 * time.Second}
			sResp, err := client.Do(statusReq)
			if err != nil {
				continue
			}

			var sRes struct {
				Status      string `json:"status"`
				BotToken    string `json:"bot_token"`
				IlinkBotID  string `json:"ilink_bot_id"`
				IlinkUserID string `json:"ilink_user_id"`
			}
			json.NewDecoder(sResp.Body).Decode(&sRes)
			sResp.Body.Close()

			switch sRes.Status {
			case "wait":
			case "scaned":
				fmt.Println("Scanned, please confirm on your phone...")
			case "expired":
				fmt.Println("QR code expired, refreshing...")
				continue outer
			case "confirmed":
				fmt.Println("Login confirmed! BotID:", sRes.IlinkBotID)

				configLock.Lock()
				user := &UserConfig{
					BotToken:    sRes.BotToken,
					BotID:       sRes.IlinkBotID,
					IlinkUserID: sRes.IlinkUserID,
					APIToken:    generateToken(16),
				}
				cfg.Bots[user.BotID] = user

				if len(cfg.Bots) == 1 {
					activeUser = user.BotID
				}
				configLock.Unlock()

				saveConfig()
				go monitorWeixin(user)
				return nil
			}
			time.Sleep(1 * time.Second)
		}
	}
}

func monitorWeixin(user *UserConfig) {
	fmt.Printf("[Bot: %s] Started listening for messages...\n", user.BotID)
	client := &http.Client{Timeout: 45 * time.Second}
	timeoutMs := 35000

	for {
		reqData := map[string]interface{}{
			"get_updates_buf": user.GetUpdatesBuf,
			"base_info": map[string]string{
				"channel_version": "1.0.0",
			},
		}
		b, _ := json.Marshal(reqData)

		req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/getupdates", bytes.NewReader(b))
		commonHeaders(req, true, user.BotToken)

		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			time.Sleep(2 * time.Second)
			continue
		}

		type MessageItem struct {
			Type     int `json:"type"`
			TextItem struct {
				Text string `json:"text"`
			} `json:"text_item"`
		}

		type WeixinMessage struct {
			FromUserID   string        `json:"from_user_id"`
			ContextToken string        `json:"context_token"`
			ItemList     []MessageItem `json:"item_list"`
		}

		var updateRes struct {
			Ret                  int             `json:"ret"`
			Errcode              int             `json:"errcode"`
			GetUpdatesBuf        string          `json:"get_updates_buf"`
			LongpollingTimeoutMs int             `json:"longpolling_timeout_ms"`
			Msgs                 []WeixinMessage `json:"msgs"`
		}

		json.Unmarshal(bodyBytes, &updateRes)

		if updateRes.Ret != 0 || updateRes.Errcode != 0 {
			time.Sleep(2 * time.Second)
			continue
		}

		if updateRes.LongpollingTimeoutMs > 0 {
			timeoutMs = updateRes.LongpollingTimeoutMs
			client.Timeout = time.Duration(timeoutMs+10000) * time.Millisecond
		}

		if updateRes.GetUpdatesBuf != "" {
			configLock.Lock()
			user.GetUpdatesBuf = updateRes.GetUpdatesBuf
			configLock.Unlock()
			saveConfig()
		}

		for _, msg := range updateRes.Msgs {
			if msg.FromUserID != "" {
				configLock.Lock()
				if msg.ContextToken != "" {
					user.ContextToken = msg.ContextToken
				}
				configLock.Unlock()
				saveConfig()
			}

			for _, item := range msg.ItemList {
				if item.Type == 1 && item.TextItem.Text != "" {
					fmt.Printf("\n[Bot: %s | Message from %s]: %s\n> ", user.BotID, msg.FromUserID, item.TextItem.Text)
				} else {
					fmt.Printf("\n[Bot: %s | Message from %s]: <Media/Other type %d>\n> ", user.BotID, msg.FromUserID, item.Type)
				}
			}
		}
	}
}

func printBots() {
	fmt.Println("Logged in bots:")
	configLock.Lock()
	i := 1
	for botID, u := range cfg.Bots {
		mark := " "
		if botID == activeUser {
			mark = "*"
		}
		fmt.Printf("  %d) [%s] BotID: %s  |  APIToken: %s\n", i, mark, botID, u.APIToken)
		i++
	}
	configLock.Unlock()
}

func consoleReader() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("\nConsole commands:")
	fmt.Println("  /login       - Scan QR code to add a new user/bot.")
	fmt.Println("  /bots        - List all logged-in bots and select active one.")
	fmt.Println("  /bot <num>   - Select bot by list index.")
	fmt.Println("  /del <num>   - Delete bot by list index.")
	fmt.Println("  [Text]       - Send message using active user to themselves.")

	for {
		if activeUser == "" {
			fmt.Print("[No Bot Selected] > ")
		} else {
			fmt.Printf("[%s] > ", activeUser)
		}

		text, err := reader.ReadString('\n')
		if err != nil {
			return // 发生错误（如 EOF）时退出控制台读取逻辑
		}
		text = strings.TrimSpace(text)

		if text == "" {
			continue
		}

		if text == "/login" {
			doQRLogin()
			continue
		}

		if text == "/bots" {
			printBots()
			fmt.Print("Enter number to select (or enter to cancel): ")
			numStr, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			numStr = strings.TrimSpace(numStr)
			if numStr != "" {
				idx, err := strconv.Atoi(numStr)
				if err == nil {
					selectBot(idx)
				}
			}
			continue
		}

		if strings.HasPrefix(text, "/bot ") {
			parts := strings.Split(text, " ")
			if len(parts) > 1 {
				idx, err := strconv.Atoi(parts[1])
				if err == nil {
					selectBot(idx)
				}
			}
			continue
		}

		if strings.HasPrefix(text, "/del ") {
			parts := strings.Split(text, " ")
			if len(parts) > 1 {
				idx, err := strconv.Atoi(parts[1])
				if err == nil {
					deleteBot(idx)
				}
			}
			continue
		}

		if strings.HasPrefix(text, "/") {
			fmt.Println("Command not recognized, treating as text msg...")
		}

		configLock.Lock()
		user, exists := cfg.Bots[activeUser]
		configLock.Unlock()

		if !exists {
			fmt.Println("No active bot selected. Type '/bots' to select.")
			continue
		}

		if user.IlinkUserID == "" || user.ContextToken == "" {
			fmt.Println("Active user has no message context to reply to. (Wait for one message or context is missing)")
			continue
		}

		err = sendMessage(user, user.IlinkUserID, text, user.ContextToken)
		if err != nil {
			fmt.Printf("Send failed: %v\n", err)
		} else {
			fmt.Println("Send success!")
		}
	}
}

func selectBot(idx int) {
	configLock.Lock()
	defer configLock.Unlock()
	i := 1
	found := false
	for botID := range cfg.Bots {
		if i == idx {
			activeUser = botID
			fmt.Printf("Active bot changed to: %s\n", botID)
			found = true
			break
		}
		i++
	}
	if !found {
		fmt.Println("Invalid bot index.")
	}
}

func deleteBot(idx int) {
	configLock.Lock()
	i := 1
	var targetBot string
	for botID := range cfg.Bots {
		if i == idx {
			targetBot = botID
			break
		}
		i++
	}

	if targetBot != "" {
		delete(cfg.Bots, targetBot)
		if activeUser == targetBot {
			activeUser = ""
		}
		fmt.Printf("Bot deleted: %s\n", targetBot)
		configLock.Unlock() // Unlock before calling saveConfig
		saveConfig()
	} else {
		configLock.Unlock()
		fmt.Println("Invalid bot index.")
	}
}

func sendMessage(user *UserConfig, to string, text string, contextToken string) error {
	reqData := map[string]interface{}{
		"msg": map[string]interface{}{
			"from_user_id":  "",
			"to_user_id":    to,
			"client_id":     fmt.Sprintf("openclaw-weixin:%d-%x", time.Now().UnixMilli(), func() []byte { b := make([]byte, 4); rand.Read(b); return b }()),
			"message_type":  2,
			"message_state": 2,
			"context_token": contextToken,
			"item_list": []map[string]interface{}{
				{
					"type": 1,
					"text_item": map[string]string{
						"text": text,
					},
				},
			},
		},
		"base_info": map[string]string{
			"channel_version": "1.0.2",
		},
	}

	b, _ := json.Marshal(reqData)
	req, _ := http.NewRequest("POST", DefaultBaseURL+"/ilink/bot/sendmessage", bytes.NewReader(b))
	commonHeaders(req, true, user.BotToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var res struct {
		Ret     int    `json:"ret"`
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
		ErrMsg  string `json:"err_msg"`
	}
	json.Unmarshal(bodyBytes, &res)

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if res.Ret != 0 || res.Errcode != 0 {
		msg := res.Errmsg
		if msg == "" {
			msg = res.ErrMsg
		}
		if msg == "" {
			msg = string(bodyBytes) // 如果没有明确的消息字段，显示完整响应体
		}
		return fmt.Errorf("API Error: ret=%d, errcode=%d, msg=%s", res.Ret, res.Errcode, msg)
	}
	return nil
}
