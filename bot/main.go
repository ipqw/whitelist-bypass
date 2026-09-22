// wlb-bot: VK-бот обхода, один на все поды.
//
// Каждый человек из конфига привязан к своему поду StatefulSet'а. Бот видит и
// пересоздаёт только его комнаты, а когда в поде появляется новая ссылка (ребут,
// падение creator'а, /recreate) — сам присылает её владельцу.
// Никакого произвольного shell: бот только ходит в HTTP агента своего пода.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	vkAPIBase    = "https://api.vk.ru/method"
	vkAPIVersion = "5.199"
	pollWait     = 25
	retryDelay   = 3 * time.Second
)

var platformName = map[string]string{
	"vk": "VK Call", "tm": "Telemost", "wb": "WB Stream", "dion": "DION",
}

type user struct {
	VKID int64  `json:"vk_id"`
	Name string `json:"name"`
	Pod  int    `json:"pod"`

	mu sync.Mutex // сериализует /recreate и рассылку новых ссылок одному человеку
}

type config struct {
	Users []*user `json:"users"`
}

type room struct {
	Platform string `json:"platform"`
	LoggedIn bool   `json:"logged_in"`
	Running  bool   `json:"running"`
	Link     string `json:"link"`
	Notified bool   `json:"notified"`
}

type bot struct {
	token, groupID string
	agentURL       string // шаблон, %d = номер пода
	users          map[int64]*user
	http           *http.Client
	longHTTP       *http.Client

	server, key, ts string
}

// ---------- VK ----------

func (b *bot) api(method string, params url.Values) (json.RawMessage, error) {
	params.Set("v", vkAPIVersion)
	params.Set("access_token", b.token)
	resp, err := b.http.PostForm(vkAPIBase+"/"+method, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Response json.RawMessage `json:"response"`
		Error    struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error.Code != 0 {
		return nil, fmt.Errorf("vk api %s: %d %s", method, out.Error.Code, out.Error.Msg)
	}
	return out.Response, nil
}

func (b *bot) getLongPollServer() error {
	p := url.Values{}
	p.Set("group_id", b.groupID)
	raw, err := b.api("groups.getLongPollServer", p)
	if err != nil {
		return err
	}
	var lp struct{ Server, Key, Ts string }
	if err := json.Unmarshal(raw, &lp); err != nil {
		return err
	}
	b.server, b.key, b.ts = lp.Server, lp.Key, lp.Ts
	return nil
}

func (b *bot) send(peerID int64, text string) error {
	p := url.Values{}
	p.Set("peer_id", fmt.Sprint(peerID))
	p.Set("message", text)
	p.Set("random_id", fmt.Sprint(time.Now().UnixNano()))
	_, err := b.api("messages.send", p)
	if err != nil {
		log.Printf("[bot] send to %d: %v", peerID, err)
	}
	return err
}

func (b *bot) poll() error {
	u := fmt.Sprintf("%s?act=a_check&key=%s&ts=%s&wait=%d", b.server, b.key, b.ts, pollWait)
	resp, err := b.longHTTP.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var data struct {
		Ts      string `json:"ts"`
		Failed  int    `json:"failed"`
		Updates []struct {
			Type   string `json:"type"`
			Object struct {
				Message struct {
					Text   string `json:"text"`
					FromID int64  `json:"from_id"`
					PeerID int64  `json:"peer_id"`
				} `json:"message"`
			} `json:"object"`
		} `json:"updates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	if data.Failed != 0 {
		return fmt.Errorf("longpoll failed=%d", data.Failed)
	}
	if data.Ts != "" {
		b.ts = data.Ts
	}
	for _, u := range data.Updates {
		if u.Type != "message_new" {
			continue
		}
		m := u.Object.Message
		go b.handleMessage(m.PeerID, m.FromID, strings.TrimSpace(m.Text))
	}
	return nil
}

// ---------- агент пода ----------

func (b *bot) agent(u *user) string { return fmt.Sprintf(b.agentURL, u.Pod) }

func (b *bot) rooms(u *user) ([]room, error) {
	resp, err := b.http.Get(b.agent(u) + "/rooms")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Rooms []room `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Rooms, nil
}

func (b *bot) recreate(u *user, p string) (room, error) {
	resp, err := b.longHTTP.Post(b.agent(u)+"/recreate/"+p, "application/json", nil)
	if err != nil {
		return room{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return room{}, fmt.Errorf("%s", e.Error)
	}
	var r room
	return r, json.NewDecoder(resp.Body).Decode(&r)
}

func (b *bot) markNotified(u *user, p, link string) {
	body, _ := json.Marshal(map[string]string{"link": link})
	resp, err := b.http.Post(b.agent(u)+"/notified/"+p, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[bot] notified %s/%s: %v", u.Name, p, err)
		return
	}
	resp.Body.Close()
}

// ---------- команды ----------

func (b *bot) handleMessage(peerID, fromID int64, text string) {
	u, ok := b.users[fromID]
	if !ok {
		return
	}
	log.Printf("[bot] %s: %q", u.Name, text)
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 {
		b.help(peerID)
		return
	}
	switch fields[0] {
	case "/rooms", "rooms", "/start", "start", "начать", "/list", "list":
		b.handleRooms(peerID, u)
	case "/recreate", "recreate", "/new", "new":
		if len(fields) < 2 {
			b.send(peerID, "Укажи платформу: /recreate wb|vk|tm|dion|all")
			return
		}
		b.handleRecreate(peerID, u, fields[1])
	default:
		b.help(peerID)
	}
}

func (b *bot) help(peerID int64) {
	b.send(peerID, "Команды:\n/rooms — текущие ссылки на комнаты\n/recreate <wb|vk|tm|dion|all> — пересоздать комнату\n\nНовые ссылки приходят сюда сами.")
}

func (b *bot) handleRooms(peerID int64, u *user) {
	rooms, err := b.rooms(u)
	if err != nil {
		b.send(peerID, "Под недоступен: "+err.Error())
		return
	}
	var lines []string
	for _, r := range rooms {
		name := platformName[r.Platform]
		switch {
		case !r.LoggedIn:
			continue
		case r.Link != "" && r.Running:
			lines = append(lines, fmt.Sprintf("✓ %s\n%s", name, r.Link))
		case r.Running:
			lines = append(lines, fmt.Sprintf("… %s — комната создаётся", name))
		default:
			lines = append(lines, fmt.Sprintf("✗ %s — creator не запущен (/recreate %s)", name, r.Platform))
		}
	}
	if len(lines) == 0 {
		b.send(peerID, "В поде нет ни одной платформы с входом в аккаунт.")
		return
	}
	b.send(peerID, strings.Join(lines, "\n\n"))
}

func (b *bot) handleRecreate(peerID int64, u *user, target string) {
	var todo []string
	switch {
	case target == "all":
		rooms, err := b.rooms(u)
		if err != nil {
			b.send(peerID, "Под недоступен: "+err.Error())
			return
		}
		for _, r := range rooms {
			if r.LoggedIn {
				todo = append(todo, r.Platform)
			}
		}
	case platformName[target] != "":
		todo = []string{target}
	default:
		b.send(peerID, "Неизвестная платформа. Доступно: wb, vk, tm, dion, all")
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, p := range todo {
		name := platformName[p]
		b.send(peerID, fmt.Sprintf("Пересоздаю %s…", name))
		r, err := b.recreate(u, p)
		if err != nil {
			b.send(peerID, fmt.Sprintf("%s: не вышло — %v", name, err))
			continue
		}
		if b.send(peerID, fmt.Sprintf("✓ %s пересоздан:\n%s", name, r.Link)) == nil {
			b.markNotified(u, p, r.Link)
		}
	}
}

// watch раз в interval сверяет ссылки во всех подах и шлёт владельцу те, что он
// ещё не получал. Отметка «отправлено» хранится в поде, не в боте.
func (b *bot) watch(interval time.Duration) {
	for {
		for _, u := range b.users {
			if !u.mu.TryLock() {
				continue // идёт /recreate — ссылку отправит он
			}
			rooms, err := b.rooms(u)
			if err != nil {
				log.Printf("[watch] %s (pod %d): %v", u.Name, u.Pod, err)
			}
			for _, r := range rooms {
				if r.Link == "" || r.Notified || !r.Running {
					continue
				}
				msg := fmt.Sprintf("Новая ссылка %s:\n%s", platformName[r.Platform], r.Link)
				if b.send(u.VKID, msg) == nil {
					b.markNotified(u, r.Platform, r.Link)
					log.Printf("[watch] %s: отправлена новая ссылка %s", u.Name, r.Platform)
				}
			}
			u.mu.Unlock()
		}
		time.Sleep(interval)
	}
}

func (b *bot) run() error {
	if err := b.getLongPollServer(); err != nil {
		return fmt.Errorf("getLongPollServer: %w", err)
	}
	log.Printf("[bot] longpoll up, users=%d", len(b.users))
	for {
		if err := b.poll(); err != nil {
			log.Printf("[bot] poll: %v", err)
			time.Sleep(retryDelay)
			if err := b.getLongPollServer(); err != nil {
				log.Printf("[bot] reconnect: %v", err)
			}
		}
	}
}

func main() {
	cfgPath := flag.String("config", "/etc/wlb/users.json", "карта людей: VK id → номер пода")
	agentURL := flag.String("agent-url", "http://whitelist-bypass-%d.whitelist-bypass:8080", "адрес агента пода, %d = номер пода")
	watchEvery := flag.Duration("watch", 20*time.Second, "как часто сверять ссылки в подах")
	flag.Parse()

	token, groupID := os.Getenv("VK_TOKEN"), os.Getenv("VK_GROUP_ID")
	if token == "" || groupID == "" {
		log.Fatal("VK_TOKEN и VK_GROUP_ID обязательны")
	}
	data, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	users := map[int64]*user{}
	for _, u := range cfg.Users {
		if u.VKID == 0 {
			log.Fatalf("config: у %q нет vk_id", u.Name)
		}
		users[u.VKID] = u
		log.Printf("[bot] %s → pod %d", u.Name, u.Pod)
	}
	if len(users) == 0 {
		log.Fatal("config: пустой список людей")
	}

	b := &bot{
		token: token, groupID: groupID, agentURL: *agentURL, users: users,
		http:     &http.Client{Timeout: 15 * time.Second},
		longHTTP: &http.Client{Timeout: 2 * time.Minute},
	}
	go b.watch(*watchEvery)
	if err := b.run(); err != nil {
		log.Fatalf("[bot] %v", err)
	}
}
