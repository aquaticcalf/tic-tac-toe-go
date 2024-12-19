package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "os"
    "sync"
    "time"

    "github.com/gorilla/websocket"
    "go.mongodb.org/mongo-driver/bson"
    "go.mongodb.org/mongo-driver/mongo"
    "go.mongodb.org/mongo-driver/mongo/options"
)

type game_state int

const (
    waiting game_state = iota
    playing
    finished
)

type move_history struct {
    Position int       `bson:"position"`
    Symbol   string    `bson:"symbol"`
    Time     time.Time `bson:"time"`
}

type game struct {
    ID            string         `bson:"_id"`
    Board         [9]string      `bson:"board"`
    Players       map[string]*player `bson:"-"`
    CurrentTurn   string         `bson:"current_turn"`
    State         game_state     `bson:"state"`
    LastActive    time.Time      `bson:"last_active"`
    MoveHistory   []move_history `bson:"move_history"`
    WinningPlayer string         `bson:"winning_player,omitempty"`
    StartTime     time.Time      `bson:"start_time"`
    mu           sync.Mutex      `bson:"-"`
}

type player struct {
    Conn       *websocket.Conn
    Symbol     string
    IsActive   bool
    JoinTime   time.Time
    MoveCount  int
}

type message struct {
    Type        string     `json:"type"`
    Position    int        `json:"position,omitempty"`
    Player      string     `json:"player,omitempty"`
    Board       [9]string  `json:"board,omitempty"`
    Turn        string     `json:"turn,omitempty"`
    Error       string     `json:"error,omitempty"`
    State       game_state `json:"state,omitempty"`
    MoveHistory []move_history `json:"move_history,omitempty"`
    TimeLeft    int        `json:"time_left,omitempty"`
}

var (
    upgrader = websocket.Upgrader{
        ReadBufferSize:  1024,
        WriteBufferSize: 1024,
        CheckOrigin: func(r *http.Request) bool {
            return true
        },
    }
    
    games = make(map[string]*game)
    games_mutex sync.Mutex
    cleanup_time = 30 * time.Minute
    move_timeout = 30 * time.Second
    
    mongo_client *mongo.Client
    games_collection *mongo.Collection
)

func init_mongodb() error {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    
    mongo_uri := os.Getenv("MONGODB_URI")
    if mongo_uri == "" {
        log.Fatal("MONGODB_URI environment variable is not set")
    }
    
    client_options := options.Client().ApplyURI(mongo_uri)
    client, err := mongo.Connect(ctx, client_options)
    if err != nil {
        return fmt.Errorf("failed to connect to mongodb: %v", err)
    }
    
    err = client.Ping(ctx, nil)
    if err != nil {
        return fmt.Errorf("failed to ping mongodb: %v", err)
    }
    
    mongo_client = client
    games_collection = client.Database("tictactoe").Collection("games")
    
    return nil
}

func main() {
    if err := init_mongodb(); err != nil {
        log.Fatal(err)
    }
    
    go cleanup_inactive_games()
    
    http.HandleFunc("/ws", handle_connections)
    http.Handle("/", http.FileServer(http.Dir("static")))
    
    log.Println("server starting on :8080")
    if err := http.ListenAndServe(":8080", nil); err != nil {
        log.Fatal("ListenAndServe: ", err)
    }
}

func get_or_create_game(game_id string) (*game, error) {
    games_mutex.Lock()
    defer games_mutex.Unlock()
    
    if g, exists := games[game_id]; exists {
        return g, nil
    }
    
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    var saved_game game
    err := games_collection.FindOne(ctx, bson.M{"_id": game_id}).Decode(&saved_game)
    
    if err == nil {
        saved_game.Players = make(map[string]*player)
        games[game_id] = &saved_game
        return &saved_game, nil
    }
    
    new_game := &game{
        ID:          game_id,
        Players:     make(map[string]*player),
        CurrentTurn: "X",
        State:       waiting,
        LastActive:  time.Now(),
        StartTime:   time.Now(),
        MoveHistory: make([]move_history, 0),
    }
    
    games[game_id] = new_game
    
    _, err = games_collection.InsertOne(ctx, new_game)
    if err != nil {
        return nil, fmt.Errorf("failed to save new game: %v", err)
    }
    
    return new_game, nil
}

func handle_move(g *game, p *player, position int) {
    g.mu.Lock()
    defer g.mu.Unlock()
    
    if g.State != playing || g.CurrentTurn != p.Symbol || 
       position < 0 || position >= 9 || g.Board[position] != "" {
        send_error(p, "invalid move")
        return
    }
    
    move := move_history{
        Position: position,
        Symbol:   p.Symbol,
        Time:     time.Now(),
    }
    
    g.Board[position] = p.Symbol
    g.MoveHistory = append(g.MoveHistory, move)
    g.CurrentTurn = get_next_turn(g.CurrentTurn)
    p.MoveCount++
    
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    update := bson.M{
        "$set": bson.M{
            "board":        g.Board,
            "current_turn": g.CurrentTurn,
            "move_history": g.MoveHistory,
            "last_active": time.Now(),
        },
    }
    
    if winner := check_winner(g.Board); winner != "" {
        g.State = finished
        g.WinningPlayer = winner
        update["$set"].(bson.M)["state"] = finished
        update["$set"].(bson.M)["winning_player"] = winner
        broadcast_game_over(g, winner)
    } else if is_board_full(g.Board) {
        g.State = finished
        update["$set"].(bson.M)["state"] = finished
        broadcast_game_over(g, "")
    } else {
        broadcast_game_state(g)
    }
    
    games_collection.UpdateOne(ctx, bson.M{"_id": g.ID}, update)
}

func check_special_win(board [9]string) string {
    patterns := map[string][]int{
        "diagonal_win":    {0, 4, 8},
        "reverse_diagonal_win": {2, 4, 6},
        "center_row_win": {3, 4, 5},
        "center_col_win": {1, 4, 7},
    }
    
    for _, positions := range patterns {
        if board[positions[0]] != "" &&
           board[positions[0]] == board[positions[1]] &&
           board[positions[1]] == board[positions[2]] {
            return board[positions[0]]
        }
    }
    return ""
}

func broadcast_game_state(g *game) {
    msg := message{
        Type:        "update",
        Board:       g.Board,
        Turn:        g.CurrentTurn,
        State:       g.State,
        MoveHistory: g.MoveHistory,
        TimeLeft:    int(move_timeout.Seconds()),
    }
    broadcast(g, msg)
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