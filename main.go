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
	ID         *string
	UniqueName *string
	isHost bool
	Conn *websocket.Conn
	Send chan []byte
}

type Game struct {
	Code  string
	Host *User
	Players map[string]*User
	HostCode string
	Mutex sync.Mutex
}

var (
	games = make(map[string]*Game)
	gamesMu sync.Mutex
)

/* returns random 9-character alphanumeric string,
with 3 letters and 6 digits, in this format: A12B34C56 */
func randomCode() string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const numbers = "0123456789"
	rand.Seed(time.Now().UnixNano())
	b := make([]byte, 9)
	for i := 0; i < 9; i += 3 {
		b[i] = letters[rand.Intn(len(letters))]
		b[i+1] = numbers[rand.Intn(len(numbers))]
		b[i+2] = numbers[rand.Intn(len(numbers))]
	}
	return string(b)
}

func createGame() *Game {
	code := randomCode()
	hostCode := randomCode()
	g := &Game{
		Code:  code,
		Players: make(map[string]*User),
		HostCode: hostCode,
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
		HostCode   *string `json:"hostCode"`
		UserID *string `json:"userId"`
		UniqueName *string `json:"uniqueName"`
	}
	if err := json.Unmarshal(msg, &init); err != nil {
		log.Error().Err(err).Msg("bad init message")
		conn.Close()
		return
	}

	var game *Game
	var user *User
	if init.Action == "host" {
		game = createGame()
		log.Trace().Msg("Created game: "+game.Code+", host code: "+game.HostCode)

		user = &User{
			isHost: true,
			Conn: conn,
			Send: make(chan []byte, 256),
		}

		// Set host for game
		game.Mutex.Lock()
		game.Host = user
		game.Mutex.Unlock()

		// Inform this user about the game code, host code, & players
		initialData := map[string]any{
			"type": "host_joined",
			"code": game.Code,
			"hostCode": game.HostCode,
			"players": func() []string {
				uniqueNames := []string{}
				for uniqueName := range game.Players {
					uniqueNames = append(uniqueNames, uniqueName)
				}
				return uniqueNames
			}(),
		}
		b, _ := json.Marshal(initialData)
		user.Conn.WriteMessage(websocket.TextMessage, b)
	} else if init.Action == "rejoin_host" {
		var ok bool
		game, ok = getGame(init.Code)
		if !ok {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Game not found","msg":"Game not found"}`))
			conn.Close()
			return
		}

		if init.HostCode == nil {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Missing host code","msg":"Failed to rejoin game as host 😭"}`))
			conn.Close()
			return
		}

		if *init.HostCode != game.HostCode {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Wrong host code !!","msg":"Failed to rejoin game as host 😭"}`))
			conn.Close()
			return
		}

		user = &User{
			isHost: true,
			Conn: conn,
			Send: make(chan []byte, 256),
		}

		// Update host
		game.Mutex.Lock()
		game.Host = user
		game.Mutex.Unlock()

		// Inform this user about the game code, host code, & players
		initialData := map[string]any{
			"type": "host_joined",
			"code": game.Code,
			"hostCode": game.HostCode,
			"players": func() []string {
				uniqueNames := []string{}
				for uniqueName := range game.Players {
					uniqueNames = append(uniqueNames, uniqueName)
				}
				return uniqueNames
			}(),
		}
		b, _ := json.Marshal(initialData)
		user.Conn.WriteMessage(websocket.TextMessage, b)
	} else if init.Action == "join" {
		var ok bool
		game, ok = getGame(init.Code)
		if !ok {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Game not found","msg":"Game not found"}`))
			conn.Close()
			return
		}

		user = &User{
			ID:   init.UserID,
			UniqueName: init.UniqueName,
			isHost: false,
			Conn: conn,
			Send: make(chan []byte, 256),
		}

		if user.UniqueName == nil {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"UniqueName can't be nil","msg":"Your name can't be blank"}`))
			conn.Close()
			return
		}

		if game.Players[*user.UniqueName] != nil {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"UniqueName already taken","msg":"That name already exists, choose a different one"}`))
			conn.Close()
			return
		}

		// Add player to game
		game.Mutex.Lock()
		game.Players[*user.UniqueName] = user
		game.Mutex.Unlock()

		// Broadcast join event
		joinMsg, _ := json.Marshal(map[string]any{
			"type": "player_joined",
			"player": *user.UniqueName,
		})
		broadcast(game, joinMsg, *user.UniqueName)

		// Inform this user about the game code & current players
		initialData := map[string]any{
			"type": "joined",
			"code": game.Code,
			"players": func() []string {
				uniqueNames := []string{}
				for uniqueName := range game.Players {
					uniqueNames = append(uniqueNames, uniqueName)
				}
				return uniqueNames
			}(),
		}
		b, _ := json.Marshal(initialData)
		user.Conn.WriteMessage(websocket.TextMessage, b)
	} else {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"Invalid action"}`))
		conn.Close()
		return
	}

	// Start goroutines
	go readPump(game, user)
	go writePump(user)
}

func readPump(game *Game, user *User) {
	defer func() {
		// Cleanup on disconnect
		game.Mutex.Lock()
		if user.isHost {
			game.Host = nil
		} else {
			delete(game.Players, *user.UniqueName)
		}
		empty := len(game.Players) == 0 &&
			game.Host == nil
		game.Mutex.Unlock()
		user.Conn.Close()

		if empty {
    		gamesMu.Lock()
    		delete(games, game.Code)
    		gamesMu.Unlock()
    		log.Trace().Msg("Game "+game.Code+" removed (no users left)")
		}

		// Notify others
		if user.isHost {
			leaveMsg, _ := json.Marshal(map[string]any{
				"type": "host_left",
			})
			broadcast(game, leaveMsg, "")
		} else {
			leaveMsg, _ := json.Marshal(map[string]any{
				"type": "player_left",
				"player": user.UniqueName,
			})
			broadcast(game, leaveMsg, "")
		}
	}()

	for {
		_, msg, err := user.Conn.ReadMessage()
		if err != nil {
			log.Debug().Err(err).Msg("read error")
			break
		}

		// Forward message to others
		broadcast(game, msg, *user.UniqueName)
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

func broadcast(game *Game, msg []byte, excludeUniqueName string) {
	game.Mutex.Lock()
	defer game.Mutex.Unlock()
	select {
	case game.Host.Send <- msg:
	default:
		// Drop message if user is lagging
	}
	for uniqueName, u := range game.Players {
		if uniqueName == excludeUniqueName {
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
