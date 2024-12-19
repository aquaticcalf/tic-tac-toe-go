package main

import (
    "log"
    "net/http"
    "sync"

    "github.com/gorilla/websocket"
)

type Game struct {
    Board      [9]string
    Players    map[string]*websocket.Conn
    CurrentTurn string
    mu         sync.Mutex
}

type Message struct {
    Type    string `json:"type"`
    Position int    `json:"position,omitempty"`
    Player  string `json:"player,omitempty"`
    Board   [9]string `json:"board,omitempty"`
    Turn    string    `json:"turn,omitempty"`
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
    gamesMutex sync.Mutex
)

func main() {
    http.HandleFunc("/ws", handleConnections)
    http.Handle("/", http.FileServer(http.Dir("static")))
    
    log.Println("Server starting on :8080")
    if err := http.ListenAndServe(":8080", nil); err != nil {
        log.Fatal("ListenAndServe: ", err)
    }
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
    gameID := r.URL.Query().Get("game")
    if gameID == "" {
        http.Error(w, "Missing game ID", http.StatusBadRequest)
        return
    }

    ws, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("upgrade error: %v", err)
        return
    }
    defer ws.Close()

    gamesMutex.Lock()
    game, exists := games[gameID]
    if !exists {
        game = &Game{
            Players: make(map[string]*websocket.Conn),
            CurrentTurn: "X",
        }
        games[gameID] = game
    }
    
    player := "O"
    if len(game.Players) == 0 {
        player = "X"
    }
    game.Players[player] = ws
    gamesMutex.Unlock()

    initMsg := Message{
        Type: "init",
        Player: player,
        Board: game.Board,
        Turn: game.CurrentTurn,
    }
    ws.WriteJSON(initMsg)

    for {
        var msg Message
        err := ws.ReadJSON(&msg)
        if err != nil {
            log.Printf("error reading message: %v", err)
            game.mu.Lock()
            delete(game.Players, player)
            game.mu.Unlock()
            break
        }

        if msg.Type == "move" {
            game.mu.Lock()
            if game.CurrentTurn == player && game.Board[msg.Position] == "" {
                game.Board[msg.Position] = player
                game.CurrentTurn = getNextTurn(game.CurrentTurn)
                
                updateMsg := Message{
                    Type: "update",
                    Board: game.Board,
                    Turn: game.CurrentTurn,
                }
                
                for _, conn := range game.Players {
                    conn.WriteJSON(updateMsg)
                }

                if winner := checkWinner(game.Board); winner != "" {
                    winMsg := Message{
                        Type: "gameover",
                        Player: winner,
                    }
                    for _, conn := range game.Players {
                        conn.WriteJSON(winMsg)
                    }
                }
            }
            game.mu.Unlock()
        }
    }
}

func getNextTurn(current string) string {
    if current == "X" {
        return "O"
    }
    return "X"
}

func checkWinner(board [9]string) string {
    // winning combinations
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