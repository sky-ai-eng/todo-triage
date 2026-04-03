package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/delegate"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	"github.com/sky-ai-eng/todo-tinder/internal/worktree"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/cmd/exec"
	"github.com/sky-ai-eng/todo-tinder/internal/poller"
	"github.com/sky-ai-eng/todo-tinder/internal/server"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"
)

const defaultPort = 3000

func main() {
	// Dual-mode dispatch: exec/status commands are CLI-only (used by Claude Code agent)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			exec.Handle(os.Args[2:])
			return
		case "status":
			exec.HandleStatus(os.Args[2:])
			return
		}
	}

	// Server mode: start HTTP server + pollers
	port := defaultPort
	noBrowser := false

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--port":
			if i+1 < len(os.Args) {
				p, err := strconv.Atoi(os.Args[i+1])
				if err != nil {
					log.Fatalf("invalid port: %s", os.Args[i+1])
				}
				port = p
				i++
			}
		case "--no-browser":
			noBrowser = true
		}
	}

	database, err := db.Open()
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Todo Tinder running at http://localhost%s\n", addr)

	if !noBrowser {
		openBrowser(fmt.Sprintf("http://localhost%s", addr))
	}

	srv := server.New(database)

	distFS, err := frontendDist()
	if err != nil {
		log.Fatalf("failed to load embedded frontend: %v", err)
	}
	srv.SetStatic(distFS)

	// Clean up any orphaned worktrees from crashed runs
	worktree.Cleanup()

	// Start pollers if credentials are configured
	cfg, _ := config.Load()
	creds, _ := auth.Load()
	startPollers(database, cfg, creds, srv.WSHub())

	// Set up delegation spawner if GitHub is configured
	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
		spawner := delegate.NewSpawner(database, ghClient, srv.WSHub(), cfg.AI.Model)
		srv.SetSpawner(spawner)
		log.Println("[delegate] spawner ready")
	}

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// startPollers launches GitHub and Jira pollers as background goroutines
// if the corresponding credentials are configured. Also starts the AI scorer.
func startPollers(database *sql.DB, cfg config.Config, creds auth.Credentials, wsHub *websocket.Hub) {
	// Start AI scoring runner with WS callbacks
	scorer := ai.NewRunner(database, ai.RunnerCallbacks{
		OnScoringStarted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_started",
				Data: map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_completed",
				Data: map[string]any{"task_ids": taskIDs},
			})
		},
	})
	scorer.Start()
	log.Println("[ai] scorer started (model: haiku)")

	// onNewTasks triggers scoring AND broadcasts to frontend
	onNewTasks := func() {
		wsHub.Broadcast(websocket.Event{
			Type: "tasks_updated",
			Data: map[string]any{},
		})
		scorer.Trigger()
	}

	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		ghUser, err := auth.ValidateGitHub(creds.GitHubURL, creds.GitHubPAT)
		if err != nil {
			log.Printf("[github] token validation failed, skipping poller: %v", err)
		} else {
			ghPoller := poller.NewGitHubPoller(creds.GitHubURL, creds.GitHubPAT, ghUser.Login, database, cfg.GitHub.PollInterval, onNewTasks)
			ghPoller.Start()
			log.Printf("[github] poller started (interval: %s, user: %s)", cfg.GitHub.PollInterval, ghUser.Login)
		}
	}

	if creds.JiraPAT != "" && creds.JiraURL != "" {
		jiraPoller := poller.NewJiraPoller(creds.JiraURL, creds.JiraPAT, cfg.Jira.Projects, database, cfg.Jira.PollInterval, onNewTasks)
		jiraPoller.Start()
		log.Printf("[jira] poller started (interval: %s, projects: %v)", cfg.Jira.PollInterval, cfg.Jira.Projects)
	}

	// Trigger an initial scoring run for any tasks already in the DB without scores
	scorer.Trigger()
}
