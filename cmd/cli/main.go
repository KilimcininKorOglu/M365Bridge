// Command line interface for M365 Copilot.
// Single binary with subcommands: serve (API server), setup-wizard (browser-based setup).
// Default mode (no subcommand) runs CLI query or interactive mode.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/servers"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/setup"
)

const (
	// defaultRefreshTokenFile is the default path for the refresh token. The
	// value is where a credential is stored, not a credential.
	// #nosec G101
	defaultRefreshTokenFile = "data/tokens/rt_90day.txt"
	// defaultCacheFile is the default path for the token cache.
	defaultCacheFile = "data/tokens/token_cache.json"
	// defaultPort is the default port for the API server.
	defaultPort = 8000
	// defaultSetupFile is the default path for the setup wizard input.
	defaultSetupFile = "data/setup.json"
	// defaultModel is the model key used when -model is not given.
	defaultModel = "auto"
)

func main() {
	// Help is answered before the logger starts, so `--help` prints the usage
	// text alone instead of a startup log line ahead of it.
	if len(os.Args) > 1 && isHelpFlag(os.Args[1]) {
		printUsage(os.Stdout)
		return
	}

	// Initialize dual-writer logger (stdout + data/proxy.log)
	if err := logging.Init(logging.LevelDebug); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logging.Close()
	logging.Infof("M365Bridge v%s starting", models.Version)

	// Check for subcommand
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServer(os.Args[2:])
			return
		case "setup-wizard":
			runSetupWizard(os.Args[2:])
			return
		case "login":
			runLogin(os.Args[2:])
			return
		}
	}

	// Default: CLI mode
	runCLI()
}

// isHelpFlag reports whether an argument asks for the usage text.
func isHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	}
	return false
}

// printUsage writes the whole command surface.
//
// The flag package prints only the flag set it was handed, so the default set
// never mentioned the subcommands and the subcommand sets never mentioned each
// other. Every invocation form is listed here instead.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `M365Bridge v%s - Microsoft 365 Copilot as an OpenAI and Anthropic API

USAGE
  m365-bridge [flags] ["question"]      Ask one question, or start interactive mode
  m365-bridge serve [flags]             Run the HTTP API server and web interface
  m365-bridge setup-wizard [flags]      Import credentials from a browser export
  m365-bridge login [flags]             Capture every credential in your own browser
  m365-bridge --help                    Print this text

  Every flag is optional. With none, serve listens on port %d and setup-wizard
  reads %s.

CLI FLAGS
  -model <name>     Model to use (default %q). See -list-models for the full list.
  -reasoning        Use the reasoning model instead of -model
  -i                Interactive mode; read questions until EOF
  -no-stream        Wait for the whole answer instead of streaming it
  -list-models      Print the available models and exit; needs no credentials
  -version          Print the version and exit

SERVE FLAGS
  -port <number>    Port to listen on (default %d)
  -version          Print the version and exit

SETUP-WIZARD FLAGS
  -file <path>      Setup JSON with oid, tenant and refresh_token (default %q)

LOGIN FLAGS
  -browser <path>   Browser executable to drive (default: discover the
                    installed Chrome automatically)

`, models.Version, defaultPort, defaultSetupFile, defaultModel, defaultPort, defaultSetupFile)

	printEnvironment(w)

	_, _ = fmt.Fprint(w, `
EXAMPLES
  m365-bridge "explain the CAP theorem"
  m365-bridge -model gpt5.5-reasoning -i
  m365-bridge serve --port 8000
  m365-bridge --list-models
  m365-bridge setup-wizard
  m365-bridge login
`)
}

// printEnvironment writes every environment variable the binary reads.
//
// The defaults come from the constants LoadConfig applies, so a changed default
// cannot leave a stale value here. The text used to name four variables and
// defer the rest to the README, which did not document the two the process
// exits without.
func printEnvironment(w io.Writer) {
	_, _ = fmt.Fprintf(w, `ENVIRONMENT
  Read from data/.env; a process environment variable takes precedence.

  Identity
    M365_TENANT_ID                 Required. Directory (tenant) ID.
    M365_USER_OID                  Required. Object ID of the signed-in user.
    M365_CLIENT_ID                 OAuth client the tokens are issued to
                                   (default %s).

  Server access
    M365_API_KEYS                  Comma-separated keys a client must present.
                                   Unset leaves every route open.
    M365_API_KEY                   A single key; read only when M365_API_KEYS
                                   is unset.
    M365_ENABLE_WEB_UI             Serve the browser interface at / and record a
                                   transcript per session (default true).
    M365_WEB_UI_PASSWORD           Password the browser interface asks for.
                                   Unset opens the interface to anyone who can
                                   reach it. The interface sends it in the same
                                   header an API client sends its key, so it is
                                   accepted wherever a key is.

  Answers
    M365_ENABLE_WEB_SEARCH         Let Copilot search the web (default true).
    M365_MAX_TOOL_ROUNDS           Tool rounds one turn may drive before the
                                   request is refused (default %d, ceiling %d).
    M365_CONTEXT_WINDOW            Context window /v1/models advertises
                                   (default %d). M365 enforces its own limits.
    M365_MAX_OUTPUT_TOKENS         Output budget /v1/models advertises
                                   (default %d).
    M365_IMAGE_HOST_ALLOWLIST      Hosts a generated image may be fetched from,
                                   comma-separated (default %s).
    TZ                             Timezone sent with each turn; falls back to
                                   the system zone, then UTC.

  Built-in coding tools, off unless enabled
    M365_ENABLE_CODE_TOOLS         Run the built-in file and command tools on
                                   this host (default false).
    M365_AUTO_EXPOSE_TOOLS         Offer them to a request that sent no tools
                                   (default false).
    M365_WORKSPACE_DIR             Directory the tools may not leave
                                   (default %q).
    M365_CODE_TOOL_TIMEOUT         Timeout for one command (default %s).
    M365_CODE_TOOL_MAX_ITERATIONS  Tool rounds inside one request (default %d).
    M365_CODE_TOOL_MAX_OUTPUT      Bytes kept from one command (default %d).
    M365_CODE_TOOL_MAX_READ_BYTES  Bytes read from one file (default %d).
`,
		models.DefaultClientID,
		models.DefaultMaxToolRounds,
		models.MaxToolRoundsCeiling,
		models.DefaultContextWindowTokens,
		models.DefaultMaxOutputTokens,
		strings.Join(models.DefaultImageHostAllowlist, ","),
		models.DefaultWorkspaceDir,
		models.DefaultCodeToolTimeout,
		models.DefaultCodeToolMaxIterations,
		models.DefaultCodeToolMaxOutput,
		models.DefaultCodeToolMaxReadBytes,
	)
}

// runServer starts the HTTP API server.
func runServer(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stdout) }
	port := fs.Int("port", defaultPort, "Port to listen on")
	showVersion := fs.Bool("version", false, "Show version")
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Printf("M365 Copilot API Server v%s\n", models.Version)
		os.Exit(0)
	}

	config := models.LoadConfig()

	if config.TenantID == "" || config.UserOID == "" {
		logging.Fatalf("Error: M365_TENANT_ID and M365_USER_OID environment variables are required")
	}

	tokenManager := auth.NewTokenManager(
		config.TenantID,
		config.ClientID,
		config.Scope,
		defaultRefreshTokenFile,
		defaultCacheFile,
	)
	tokenManager.SetUserOID(config.UserOID)

	apiServer := servers.NewAPIServer(config, tokenManager)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		if err := apiServer.Start(*port); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-sigChan:
		logging.Info("Shutting down server...")
		if err := apiServer.Stop(); err != nil {
			logging.Errorf("Error stopping server: %v", err)
		}
		logging.Info("Server stopped")
	case err := <-errChan:
		logging.Fatalf("Server error: %v", err)
	}
}

// runSetupWizard runs the browser-based setup wizard.
func runSetupWizard(args []string) {
	fs := flag.NewFlagSet("setup-wizard", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stdout) }
	file := fs.String("file", defaultSetupFile, "Path to setup JSON file containing oid, tenant, and refresh_token")
	_ = fs.Parse(args)

	if err := setup.Run(*file); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runLogin drives the user's own browser through the M365 sign-in and
// captures every credential the gateway keeps in one pass.
func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stdout) }
	browser := fs.String("browser", "", "Browser executable to drive; empty discovers the installed Chrome")
	_ = fs.Parse(args)

	if err := setup.RunLogin(setup.LoginOptions{BrowserPath: *browser}); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runCLI runs the default CLI mode (single query or interactive).
func runCLI() {
	// Parse command-line flags. The model list is not repeated here because it
	// would go stale against ModelRegistry; -list-models prints the live set.
	flag.Usage = func() { printUsage(os.Stdout) }
	model := flag.String("model", defaultModel, "Model to use; -list-models prints the available names")
	reasoning := flag.Bool("reasoning", false, "Use reasoning mode")
	interactive := flag.Bool("i", false, "Interactive mode")
	noStream := flag.Bool("no-stream", false, "Disable streaming")
	listModels := flag.Bool("list-models", false, "List available models")
	showVersion := flag.Bool("version", false, "Show version")

	flag.Parse()

	// Handle version flag
	if *showVersion {
		fmt.Printf("M365Bridge v%s\n", models.Version)
		os.Exit(0)
	}

	// The model list comes from the registry compiled into this binary, so it
	// must not require credentials the way a question does.
	if *listModels {
		servers.PrintModels(os.Stdout)
		os.Exit(0)
	}

	// Load configuration
	config := models.LoadConfig()

	// Validate required configuration
	if config.TenantID == "" || config.UserOID == "" {
		fmt.Fprintf(os.Stderr, "Error: M365_TENANT_ID and M365_USER_OID environment variables are required\n")
		fmt.Fprintf(os.Stderr, "\nGet them from: https://graph.microsoft.com/v1.0/me (id and tenantId)\n")
		fmt.Fprintf(os.Stderr, "\nOr run the setup wizard to configure automatically\n")
		os.Exit(1)
	}

	// Initialize token manager
	tokenManager := auth.NewTokenManager(
		config.TenantID,
		config.ClientID,
		config.Scope,
		defaultRefreshTokenFile,
		defaultCacheFile,
	)

	// Create CLI server
	cliServer := servers.NewCLIServer(config, tokenManager)
	defer func() { _ = cliServer.Close() }()

	// Prepare options
	options := &servers.CLIOptions{
		Model:       *model,
		Reasoning:   *reasoning,
		Interactive: *interactive,
		NoStream:    *noStream,
		Prompt:      flag.Arg(0),
		ListModels:  *listModels,
	}

	// Run CLI
	if err := cliServer.Run(options); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
