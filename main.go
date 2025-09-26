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
	Studyset map[string]interface{}
	Mutex sync.Mutex
}

var (
	games = make(map[string]*Game)
	gamesMu sync.Mutex
)

/* returns random 9-character alphanumeric string,
with 3 letters and 6 digits, in this format: A123B456 (LDDDLDDD)
676,000,000 possible codes */
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

/* returns 9-character alphanumeric string, with 2 letters & 3 numbers randomized
3 letters and 6 digits, in this format: A100B230 (LD00LDD0)
676,000 possible codes */
func simpleRandomCode() string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const numbers = "123456789" /* notice no zero in the randomly chosen digits, unlike randomCode() */
	rand.Seed(time.Now().UnixNano())
	b := []byte{
		letters[rand.Intn(len(letters))],
		numbers[rand.Intn(len(numbers))],
		'0',
		'0',
		letters[rand.Intn(len(letters))],
		numbers[rand.Intn(len(numbers))],
		numbers[rand.Intn(len(numbers))],
		'0',
	}
	return string(b)
}

func createGame() *Game {
	code := simpleRandomCode()
	for i := 0; games[code] != nil; i++ {
		code = randomCode()
		if i > 100 {
			log.Error().Msg("Took more than 100 iterations to generate a non-duplicate code")
			break
		}
	}
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
		Studyset map[string]interface{} `json:"studyset"`
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

		game.Mutex.Lock()
		game.Host = user
		game.Studyset = init.Studyset
		game.Mutex.Unlock()

		// Inform this user about the game code, host code, & players
		initialData := map[string]any{
			"type": "host_joined",
			"code": game.Code,
			"hostCode": game.HostCode,
			"studyset": game.Studyset,
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
			"studyset": game.Studyset,
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
		broadcast(game, joinMsg, *user.UniqueName, false)

		// Inform this user about the game code & current players
		initialData := map[string]any{
			"type": "joined",
			"code": game.Code,
			"studyset": game.Studyset,
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
			broadcast(game, leaveMsg, "", true)
		} else {
			leaveMsg, _ := json.Marshal(map[string]any{
				"type": "player_left",
				"player": user.UniqueName,
			})
			broadcast(game, leaveMsg, "", false)
		}
	}()

	for {
		_, msg, err := user.Conn.ReadMessage()
		if err != nil {
			log.Debug().Err(err).Msg("read error")
			break
		}

		// Forward message to others
		if user.isHost {
			broadcast(game, msg, "", true)
		} else {
			broadcast(game, msg, *user.UniqueName, false)
		}
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

/* set excludeUniqueName to your own UniqueName to prevent broadcasting to yourself,
when you're the host, use excludeHost to prevent broadcasting to yourself
you can leave excludeUniqueName blank (empty string), "", to not exclude any uniqueNames
(an actual uniquename can never be empty, and hosts don't have uniquenames) */
func broadcast(game *Game, msg []byte, excludeUniqueName string, excludeHost bool) {
	game.Mutex.Lock()
	defer game.Mutex.Unlock()
	if !excludeHost && game.Host != nil {
		select {
		case game.Host.Send <- msg:
		default:
			// Drop message if user is lagging
		}
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

func statsHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"currentGamesCount": len(games),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Error().Err(err).Msg("Error encoding JSON in stats handler")
		http.Error(w, err.Error(), 500)
		return
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
	http.HandleFunc("/stats", statsHandler)

	log.Info().Msg(
		"Quizfreely Realtime Server running on :" + port,
	)
	log.Fatal().Err(
		http.ListenAndServe(":"+port, nil),
	).Msgf("Error starting server")
}
