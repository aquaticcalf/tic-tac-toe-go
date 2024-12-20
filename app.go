package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/sessions"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	supa "github.com/nedpals/supabase-go"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

type GameState int

const (
	Waiting GameState = iota
	Playing
	Finished
)

type Player struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Symbol    string `json:"symbol"`
	AvatarURL string `json:"avatar_url"`
	Token     string `json:"token"`
}

type Game struct {
	ID           string             `json:"id"`
	Board        [9]string          `json:"board"`
	Players      map[string]*Player `json:"players"`
	CurrentTurn  string             `json:"current_turn"`
	State        GameState          `json:"state"`
	LastActive   time.Time          `json:"last_active"`
	WinningCells []int              `json:"winning_cells,omitempty"`
	mu           sync.Mutex
}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	games      = make(map[string]*Game)
	gamesMutex sync.RWMutex

	supabaseClient *supa.Client

	oauthConfig *oauth2.Config

	store = sessions.NewCookieStore([]byte("your-secret-key"))
)

var connections = make(map[string]*websocket.Conn)

func init() {
	// Load .env file first
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found")
	}

	// Setup logging
	logFile, err := os.OpenFile("oauth_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)

	// Initialize OAuth config AFTER environment variables are loaded
	oauthConfig = &oauth2.Config{
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		RedirectURL:  "https://games.aqclf.xyz/tictactoe/auth/github/callback",
		Scopes:       []string{"user"},
		Endpoint:     github.Endpoint,
	}

	// Initialize Supabase client with service role key
	url := os.Getenv("SUPABASE_URL")
	key := os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	supabaseClient = supa.CreateClient(url, key)

	log.Printf("Initialized Supabase client with URL: %s", url)
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("static")))
	http.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/auth.html")
	})
	http.HandleFunc("/auth/github/callback", handleGitHubCallback)
	http.HandleFunc("/api/user", handleUserCheck)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/logout", handleLogout)
	http.HandleFunc("/ws", handleWebSocket)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received GitHub callback request")
	
	code := r.URL.Query().Get("code")
	gameID := r.URL.Query().Get("state")
	
	if code == "" {
		log.Printf("Error: Missing OAuth code")
		http.Error(w, "Missing OAuth code", http.StatusBadRequest)
		return
	}

	// Exchange code for token
	token, err := oauthConfig.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("Error exchanging OAuth code: %v", err)
		http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
		return
	}

	// Get GitHub user info
	client := oauthConfig.Client(r.Context(), token)
	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		log.Printf("Error fetching GitHub user info: %v", err)
		http.Error(w, "Failed to get user info", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var githubUser map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&githubUser); err != nil {
		log.Printf("Error decoding GitHub response: %v", err)
		http.Error(w, "Failed to parse user info", http.StatusInternalServerError)
		return
	}

	// Safely convert github_id to int64
	githubID, ok := githubUser["id"].(float64)
	if !ok {
		log.Printf("Error: Invalid GitHub ID format")
		http.Error(w, "Invalid GitHub user data", http.StatusInternalServerError)
		return
	}

	// Check if user exists
	var existingUser []map[string]interface{}
	err = supabaseClient.DB.From("github_users").
		Select("*").
		Eq("github_id", fmt.Sprintf("%d", int64(githubID))).
		Execute(&existingUser)

	if err != nil {
		log.Printf("Error checking existing user: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Prepare user data
	userData := map[string]interface{}{
		"github_id":  int64(githubID),
		"username":   githubUser["login"],
		"name":       githubUser["name"],
		"avatar_url": githubUser["avatar_url"],
		"github_url": githubUser["html_url"],
		"updated_at": time.Now().UTC(),
	}

	var result []map[string]interface{}
	if len(existingUser) > 0 {
		// Update existing user
		log.Printf("Updating existing user with GitHub ID: %d", int64(githubID))
		err = supabaseClient.DB.From("github_users").
			Update(userData).
			Eq("github_id", fmt.Sprintf("%d", int64(githubID))).
			Execute(&result)
	} else {
		// Insert new user
		log.Printf("Creating new user with GitHub ID: %d", int64(githubID))
		err = supabaseClient.DB.From("github_users").
			Insert(userData).
			Execute(&result)
	}

	if err != nil {
		log.Printf("Error storing user data: %v", err)
		http.Error(w, "Failed to store user", http.StatusInternalServerError)
		return
	}

	if len(result) == 0 {
		log.Printf("Error: Empty result after user operation")
		http.Error(w, "Failed to store user", http.StatusInternalServerError)
		return
	}

	// Create session
	session, err := store.Get(r, "session-name")
	if err != nil {
		log.Printf("Error getting session: %v", err)
		http.Error(w, "Session error", http.StatusInternalServerError)
		return
	}

	session.Values["authenticated"] = true
	session.Values["user_id"] = result[0]["id"]
	
	if err := session.Save(r, w); err != nil {
		log.Printf("Error saving session: %v", err)
		http.Error(w, "Failed to save session", http.StatusInternalServerError)
		return
	}

	// Redirect with game ID if present
	redirectURL := "/"
	if gameID != "" {
		redirectURL += "?game=" + gameID
	}

	log.Printf("Successfully processed GitHub callback for user ID: %v", result[0]["id"])
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
}

func handleUserCheck(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "session-name")
	if err != nil {
		http.Error(w, "Session error", http.StatusInternalServerError)
		return
	}

	if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated": true,
	})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	gameID := r.URL.Query().Get("game")
	token := r.URL.Query().Get("token")
	if gameID == "" || token == "" {
		http.Error(w, "Missing game ID or token", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Get player from Supabase
	var players []Player
	err = supabaseClient.DB.From("players").Select("*").Eq("token", token).Execute(&players)
	if err != nil || len(players) == 0 {
		return
	}
	player := players[0]

	game := getOrCreateGame(gameID)
	game.mu.Lock()

	// Assign symbol to player
	if len(game.Players) == 0 {
		player.Symbol = "X"
	} else if len(game.Players) == 1 {
		player.Symbol = "O"
		game.State = Playing
	} else {
		game.mu.Unlock()
		conn.WriteJSON(map[string]string{"type": "error", "error": "Game is full"})
		return
	}

	game.Players[player.ID] = &player
	connections[player.ID] = conn
	game.mu.Unlock()

	// Send initial game state
	conn.WriteJSON(map[string]interface{}{
		"type":   "init",
		"player": player.Symbol,
		"board":  game.Board,
		"turn":   game.CurrentTurn,
		"state":  game.State,
	})

	// Handle player messages
	for {
		var msg struct {
			Type     string `json:"type"`
			Position int    `json:"position"`
		}

		if err := conn.ReadJSON(&msg); err != nil {
			break
		}

		if msg.Type == "move" {
			handleMove(game, &player, msg.Position)
		} else if msg.Type == "new_game" {
			handleNewGame(game)
		}
	}

	// Clean up when player disconnects
	game.mu.Lock()
	delete(game.Players, player.ID)
	delete(connections, player.ID)
	if len(game.Players) == 0 {
		gamesMutex.Lock()
		delete(games, gameID)
		gamesMutex.Unlock()
	}
	game.mu.Unlock()
}

func getOrCreateGame(gameID string) *Game {
	gamesMutex.Lock()
	defer gamesMutex.Unlock()

	if game, exists := games[gameID]; exists {
		return game
	}

	game := &Game{
		ID:          gameID,
		Players:     make(map[string]*Player),
		CurrentTurn: "X",
		State:       Waiting,
		LastActive:  time.Now(),
	}

	games[gameID] = game
	return game
}

func handleMove(game *Game, player *Player, position int) {
	game.mu.Lock()
	defer game.mu.Unlock()

	if game.State != Playing ||
		game.CurrentTurn != player.Symbol ||
		position < 0 || position >= 9 ||
		game.Board[position] != "" {
		return
	}

	game.Board[position] = player.Symbol
	game.LastActive = time.Now()

	if winner := checkWinner(game.Board); winner != "" {
		game.State = Finished
		game.WinningCells = getWinningCells(game.Board)
		broadcastGameOver(game, winner)
		updateStats(player.ID, "win")
	} else if isBoardFull(game.Board) {
		game.State = Finished
		broadcastGameOver(game, "")
		updateStats(player.ID, "draw")
	} else {
		game.CurrentTurn = getNextTurn(game.CurrentTurn)
		broadcastGameState(game)
	}
}

func checkWinner(board [9]string) string {
	lines := [][3]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, // rows
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8}, // columns
		{0, 4, 8}, {2, 4, 6}, // diagonals
	}

	for _, line := range lines {
		if board[line[0]] != "" &&
			board[line[0]] == board[line[1]] &&
			board[line[1]] == board[line[2]] {
			return board[line[0]]
		}
	}
	return ""
}

func getWinningCells(board [9]string) []int {
	lines := [][3]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8},
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8},
		{0, 4, 8}, {2, 4, 6},
	}

	for _, line := range lines {
		if board[line[0]] != "" &&
			board[line[0]] == board[line[1]] &&
			board[line[1]] == board[line[2]] {
			return []int{line[0], line[1], line[2]}
		}
	}
	return nil
}

func broadcastGameState(game *Game) {
	msg := map[string]interface{}{
		"type":  "update",
		"board": game.Board,
		"turn":  game.CurrentTurn,
		"state": game.State,
	}

	for _, player := range game.Players {
		if conn, ok := connections[player.ID]; ok {
			conn.WriteJSON(msg)
		}
	}
}

func broadcastGameOver(game *Game, winner string) {
	msg := map[string]interface{}{
		"type":          "gameover",
		"board":         game.Board,
		"winner":        winner,
		"winning_cells": game.WinningCells,
	}

	for _, player := range game.Players {
		if conn, ok := connections[player.ID]; ok {
			conn.WriteJSON(msg)
		}
	}
}

func getNextTurn(current string) string {
	if current == "X" {
		return "O"
	}
	return "X"
}

func isBoardFull(board [9]string) bool {
	for _, cell := range board {
		if cell == "" {
			return false
		}
	}
	return true
}

func handleNewGame(game *Game) {
	game.mu.Lock()
	defer game.mu.Unlock()

	// Reset the game state
	game.Board = [9]string{}
	game.CurrentTurn = "X"
	game.State = Waiting
	game.LastActive = time.Now()
	game.WinningCells = nil

	// Send the updated game state to all connected clients
	broadcastGameState(game)
}

func updateStats(playerID string, result string) {
	var stats []struct {
		Wins   int `json:"wins"`
		Losses int `json:"losses"`
		Draws  int `json:"draws"`
	}

	err := supabaseClient.DB.From("stats").Select("*").Eq("player_id", playerID).Execute(&stats)
	if err != nil || len(stats) == 0 {
		return
	}

	currentStats := stats[0]
	switch result {
	case "win":
		currentStats.Wins++
	case "loss":
		currentStats.Losses++
	case "draw":
		currentStats.Draws++
	}

	var upsertResult map[string]interface{}
	err = supabaseClient.DB.From("stats").Upsert(map[string]interface{}{
		"player_id": playerID,
		"wins":      currentStats.Wins,
		"losses":    currentStats.Losses,
		"draws":     currentStats.Draws,
	}).Execute(&upsertResult)
	if err != nil {
	}
}

func handleUserInfo(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if token == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var players []Player
	err := supabaseClient.DB.From("players").Select("*").Eq("token", token).Execute(&players)
	if err != nil || len(players) == 0 {
		http.Error(w, "Player not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(players[0])
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	playerID := r.URL.Query().Get("player_id")
	if playerID == "" {
		http.Error(w, "Player ID is required", http.StatusBadRequest)
		return
	}

	var stats []struct {
		Wins   int `json:"wins"`
		Losses int `json:"losses"`
		Draws  int `json:"draws"`
	}

	err := supabaseClient.DB.From("stats").Select("*").Eq("player_id", playerID).Execute(&stats)
	if err != nil || len(stats) == 0 {
		http.Error(w, "Stats not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(stats[0])
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if token == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var result map[string]interface{}
	err := supabaseClient.DB.From("players").Delete().Eq("token", token).Execute(&result)
	if err != nil {
		http.Error(w, "Failed to delete player", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}
