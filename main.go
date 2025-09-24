package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"sync"
	"time"
	"os"
	"strings"

    "github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/gorilla/websocket"
)

type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type User struct {
	ID   string
	Conn *websocket.Conn
	Send chan []byte
}

type Game struct {
	Code  string
	Users map[string]*User
	Mutex sync.Mutex
}

var (
	games = make(map[string]*Game)
	gamesMu sync.Mutex
)

func randomCode(n int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rand.Seed(time.Now().UnixNano())
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func createGame() *Game {
	code := randomCode(5)
	g := &Game{
		Code:  code,
		Users: make(map[string]*User),
	}
	gamesMu.Lock()
	games[code] = g
	gamesMu.Unlock()
	return g
}

func getGame(code string) (*Game, bool) {
	gamesMu.Lock()
	defer gamesMu.Unlock()
	g, ok := games[code]
	return g, ok
}

var upgrader websocket.Upgrader

func init() {
	_ = godotenv.Load()

	logLevel := os.Getenv("LOG_LEVEL")
	switch logLevel {
	case "TRACE":
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case "DEBUG":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "INFO":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "WARN":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "ERROR":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "FATAL":
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	case "PANIC":
		zerolog.SetGlobalLevel(zerolog.PanicLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	if os.Getenv("PRETTY_LOG") == "true" {
		log.Logger = log.Output(
			zerolog.ConsoleWriter{Out: os.Stderr},
		)
	} else {
		zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	}

	raw := os.Getenv("ALLOWED_ORIGINS")
	if raw == "" {
		log.Fatal().Msgf("ALLOWED_ORIGINS is empty! Check `.env`")
	} else if raw == "*" {
		upgrader = websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true;
			},
		}
	} else {
		allowed := strings.Split(raw, ",")

		upgrader = websocket.Upgrader{
    	    CheckOrigin: func(r *http.Request) bool {
    	        origin := r.Header.Get("Origin")
    	        for _, o := range allowed {
					if strings.TrimSpace(o) == origin {
    	            	return true
    	            }
    	        }
    	        return false
    	    },
    	}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("upgrade error")
		return
	}

	// Expect the client to send initial join/host request
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Error().Err(err).Msg("initial read error")
		conn.Close()
		return
	}

	var init struct {
		Action string `json:"action"` // "host" or "join"
		Code   string `json:"code"`
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(msg, &init); err != nil {
		log.Error().Err(err).Msg("bad init message")
		conn.Close()
		return
	}

	var game *Game
	if init.Action == "host" {
		game = createGame()
		log.Trace().Msg("Created game: "+game.Code)
	} else if init.Action == "join" {
		var ok bool
		game, ok = getGame(init.Code)
		if !ok {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Game not found"}`))
			conn.Close()
			return
		}
	} else {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Invalid action"}`))
		conn.Close()
		return
	}

	user := &User{
		ID:   init.UserID,
		Conn: conn,
		Send: make(chan []byte, 256),
	}

	// Add user to game
	game.Mutex.Lock()
	game.Users[user.ID] = user
	game.Mutex.Unlock()

	// Inform this user about the game code & current players
	initialData := map[string]any{
		"type": "joined",
		"code": game.Code,
		"users": func() []string {
			ids := []string{}
			for id := range game.Users {
				ids = append(ids, id)
			}
			return ids
		}(),
	}
	b, _ := json.Marshal(initialData)
	user.Conn.WriteMessage(websocket.TextMessage, b)

	// Broadcast join event
	joinMsg, _ := json.Marshal(map[string]any{
		"type": "user_joined",
		"user": user.ID,
	})
	broadcast(game, joinMsg, user.ID)

	// Start goroutines
	go readPump(game, user)
	go writePump(user)
}

func readPump(game *Game, user *User) {
	defer func() {
		// Cleanup on disconnect
		game.Mutex.Lock()
		delete(game.Users, user.ID)
		game.Mutex.Unlock()
		user.Conn.Close()

		// Notify others
		leaveMsg, _ := json.Marshal(map[string]any{
			"type": "user_left",
			"user": user.ID,
		})
		broadcast(game, leaveMsg, "")
	}()

	for {
		_, msg, err := user.Conn.ReadMessage()
		if err != nil {
			log.Error().Err(err).Msg("read error")
			break
		}

		// Forward message to others
		broadcast(game, msg, user.ID)
	}
}

func writePump(user *User) {
	for msg := range user.Send {
		err := user.Conn.WriteMessage(websocket.TextMessage, msg)
		if err != nil {
			log.Error().Err(err).Msg("write error")
			break
		}
	}
}

func broadcast(game *Game, msg []byte, excludeID string) {
	game.Mutex.Lock()
	defer game.Mutex.Unlock()
	for id, u := range game.Users {
		if id == excludeID {
			continue
		}
		select {
		case u.Send <- msg:
		default:
			// Drop message if user is lagging
		}
	}
}

const defaultPort = "8000"
func main() {
	/* godotenv is loaded in init(), above */
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	http.HandleFunc("/ws", wsHandler)

	log.Info().Msg(
		"Quizfreely Realtime Server running on :" + port,
	)
	log.Fatal().Err(
		http.ListenAndServe(":"+port, nil),
	).Msgf("Error starting server")
}
