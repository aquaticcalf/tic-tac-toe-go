package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "sync"
    "time"

    "github.com/gorilla/websocket"
    "github.com/supabase-community/supabase-go"
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
    ID            string               `json:"id"`
    Board         [9]string           `json:"board"`
    Players       map[string]*Player  `json:"players"`
    CurrentTurn   string             `json:"current_turn"`
    State         GameState          `json:"state"`
    LastActive    time.Time          `json:"last_active"`
    WinningCells  []int             `json:"winning_cells,omitempty"`
    mu            sync.Mutex
}

var (
    upgrader = websocket.Upgrader{
        ReadBufferSize:  1024,
        WriteBufferSize: 1024,
        CheckOrigin: func(r *http.Request) bool {
            return true
        },
    }

    games = make(map[string]*Game)
    gamesMutex sync.RWMutex
    
    supabaseClient *supabase.Client
    
    oauthConfig = &oauth2.Config{
        ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
        ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
        Endpoint:     github.Endpoint,
        RedirectURL:  "http://localhost:8080/auth/github/callback",
        Scopes:       []string{"user"},
    }
)

var connections = make(map[string]*websocket.Conn)

func init() {
    url := os.Getenv("SUPABASE_URL")
    key := os.Getenv("SUPABASE_KEY")
    supabaseClient = supabase.CreateClient(url, key)
}

func main() {
    http.Handle("/", http.FileServer(http.Dir("static")))
    http.HandleFunc("/auth/github/callback", handleGitHubCallback)
    http.HandleFunc("/api/user", handleUserInfo)
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
    code := r.URL.Query().Get("code")
    token, err := oauthConfig.Exchange(r.Context(), code)
    if err!= nil {
        http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
        return
    }

    client := oauthConfig.Client(r.Context(), token)
    resp, err := client.Get("https://api.github.com/user")
    if err!= nil {
        http.Error(w, "Failed to get user info", http.StatusInternalServerError)
        return
    }
    defer resp.Body.Close()

    var githubUser struct {
        ID        int    `json:"id"`
        Login     string `json:"login"`
        AvatarURL string `json:"avatar_url"`
    }
    
    if err := json.NewDecoder(resp.Body).Decode(&githubUser); err!= nil {
        http.Error(w, "Failed to decode user info", http.StatusInternalServerError)
        return
    }

    player := Player{
        ID:        fmt.Sprintf("%d", githubUser.ID),
        Name:      githubUser.Login,
        AvatarURL: githubUser.AvatarURL,
        Token:     token.AccessToken,
    }

    // Store player in Supabase
    ctx := context.Background()
    _, err = supabaseClient.From("players").Insert(ctx, []Player{player})
    if err!= nil {
        http.Error(w, "Failed to store user", http.StatusInternalServerError)
        return
    }

    // Set session cookie
    http.SetCookie(w, &http.Cookie{
        Name:     "session_token",
        Value:    token.AccessToken,
        Path:     "/",
        HttpOnly: true,
        Secure:   r.TLS!= nil,
        SameSite: http.SameSiteLaxMode,
    })

    http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    gameID := r.URL.Query().Get("game")
    token := r.URL.Query().Get("token")
    if gameID == "" || token == "" {
        http.Error(w, "Missing game ID or token", http.StatusBadRequest)
        return
    }   

    conn, err := upgrader.Upgrade(w, r, nil)
    if err!= nil {
        log.Printf("WebSocket upgrade error: %v", err)
        return
    }
    defer conn.Close()

    // Get player from Supabase
    ctx := context.Background()
    var player Player
    err = supabaseClient.From("players").Select("*").Eq("token", token).Single(ctx).Scan(&player)
    if err!= nil {
        log.Printf("Failed to get player: %v", err)
        return
    }

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

        if err := conn.ReadJSON(&msg); err!= nil {
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
        State:      Waiting,
        LastActive: time.Now(),
    }
    
    games[gameID] = game
    return game
}

func handleMove(game *Game, player *Player, position int) {
    game.mu.Lock()
    defer game.mu.Unlock()

    if game.State!= Playing || 
       game.CurrentTurn!= player.Symbol || 
       position < 0 || position >= 9 || 
       game.Board[position]!= "" {
        return
    }

    game.Board[position] = player.Symbol
    game.LastActive = time.Now()

    if winner := checkWinner(game.Board); winner!= "" {
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
        {0, 4, 8}, {2, 4, 6},            // diagonals
    }

    for _, line := range lines {
        if board[line[0]]!= "" &&
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
        if board[line[0]]!= "" &&
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
        "type":         "gameover",
        "board":        game.Board,
        "winner":       winner,
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
    ctx := context.Background()
    var stats struct {
        Wins   int `json:"wins"`
        Losses int `json:"losses"`
        Draws  int `json:"draws"`
    }

    err := supabaseClient.From("stats").Select("*").Eq("player_id", playerID).Single(ctx).Scan(&stats)
    if err!= nil {
        return
    }

    switch result {
    case "win":
        stats.Wins++
    case "loss":
        stats.Losses++
    case "draw":
        stats.Draws++
    }

    _, err = supabaseClient.From("stats").Upsert(ctx, map[string]interface{}{
        "player_id": playerID,
        "wins":     stats.Wins,
        "losses":   stats.Losses,
        "draws":    stats.Draws,
    })
    if err!= nil {
        log.Printf("Failed to update stats: %v", err)
    }
}

func handleUserInfo(w http.ResponseWriter, r *http.Request) {
    token := r.Header.Get("Authorization")
    if token == "" {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    ctx := context.Background()
    var player Player
    err := supabaseClient.From("players").Select("*").Eq("token", token).Single(ctx).Scan(&player)
    if err!= nil {
        http.Error(w, "Player not found", http.StatusNotFound)
        return
    }

    json.NewEncoder(w).Encode(player)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
    playerID := r.URL.Query().Get("player_id")
    if playerID == "" {
        http.Error(w, "Player ID is required", http.StatusBadRequest)
        return
    }

    ctx := context.Background()
    var stats struct {
        Wins   int `json:"wins"`
        Losses int `json:"losses"`
        Draws  int `json:"draws"`
    }

    err := supabaseClient.From("stats").Select("*").Eq("player_id", playerID).Single(ctx).Scan(&stats)
    if err!= nil {
        http.Error(w, "Stats not found", http.StatusNotFound)
        return
    }

    json.NewEncoder(w).Encode(stats)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
    token := r.Header.Get("Authorization")
    if token == "" {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    ctx := context.Background()
    _, err := supabaseClient.From("players").Delete(ctx, supabase.Eq("token", token))
    if err!= nil {
        http.Error(w, "Failed to delete player", http.StatusInternalServerError)
        return
    }

    http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}