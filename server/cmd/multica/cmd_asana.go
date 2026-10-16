package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var asanaCmd = &cobra.Command{
	Use:   "asana",
	Short: "Connect Asana to the workspace",
	Long: "Manages the Asana integration for the current workspace. A connection stores an Asana " +
		"access token (encrypted on the server) and is the prerequisite for mirroring Asana projects " +
		"into Multica issues that agents can own.",
}

var asanaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Asana connections",
	Args:  cobra.NoArgs,
	RunE:  runAsanaList,
}

var asanaConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect an Asana workspace with a personal access token",
	Long: "Validates the token against Asana and stores it encrypted on the server. When the token can " +
		"see several Asana workspaces, pass --asana-workspace <gid>; the command lists the choices otherwise. " +
		"Reconnecting the same Asana workspace replaces the stored token.",
	Args: cobra.NoArgs,
	RunE: runAsanaConnect,
}

var asanaDisconnectCmd = &cobra.Command{
	Use:   "disconnect <connection-id>",
	Short: "Remove an Asana connection and its project bindings",
	Long:  "Removes the connection, its project bindings, and task links. Issues already mirrored into Multica stay.",
	Args:  exactArgs(1),
	RunE:  runAsanaDisconnect,
}

func init() {
	asanaListCmd.Flags().String("output", "table", "Output format: table or json")

	asanaConnectCmd.Flags().String("token", "", "Asana personal access token. Visible to shell history and 'ps'; prefer --token-stdin or --token-file.")
	asanaConnectCmd.Flags().Bool("token-stdin", false, "Read the token from stdin. Mutually exclusive with --token and --token-file.")
	asanaConnectCmd.Flags().String("token-file", "", "Read the token from a file path (suggested mode: 0600). Mutually exclusive with --token and --token-stdin.")
	asanaConnectCmd.Flags().String("asana-workspace", "", "Asana workspace gid to connect. Required when the token can see more than one.")
	asanaConnectCmd.Flags().String("output", "json", "Output format: table or json")

	asanaCmd.AddCommand(asanaListCmd)
	asanaCmd.AddCommand(asanaConnectCmd)
	asanaCmd.AddCommand(asanaDisconnectCmd)
}

type asanaConnection struct {
	ID                 string `json:"id"`
	WorkspaceID        string `json:"workspace_id"`
	AsanaWorkspaceGID  string `json:"asana_workspace_gid"`
	AsanaWorkspaceName string `json:"asana_workspace_name"`
	AsanaUserGID       string `json:"asana_user_gid"`
	AsanaUserName      string `json:"asana_user_name"`
	AsanaUserEmail     string `json:"asana_user_email"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

type asanaListResponse struct {
	Connections []asanaConnection `json:"connections"`
	Configured  bool              `json:"configured"`
	CanManage   bool              `json:"can_manage"`
}

// resolveAsanaToken collects --token, --token-stdin, and --token-file. The
// three channels are mutually exclusive; stdin and file exist to keep the
// token out of shell history and 'ps'.
func resolveAsanaToken(cmd *cobra.Command) (string, error) {
	inline := cmd.Flags().Changed("token")
	fromStdin, _ := cmd.Flags().GetBool("token-stdin")
	fromFile := cmd.Flags().Changed("token-file")

	count := 0
	for _, set := range []bool{inline, fromStdin, fromFile} {
		if set {
			count++
		}
	}
	switch {
	case count == 0:
		return "", fmt.Errorf("specify the token via --token, --token-stdin, or --token-file")
	case count > 1:
		return "", fmt.Errorf("--token, --token-stdin, and --token-file are mutually exclusive; pick one")
	}

	var raw string
	switch {
	case inline:
		raw, _ = cmd.Flags().GetString("token")
	case fromStdin:
		buf, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read --token-stdin: %w", err)
		}
		raw = string(buf)
	case fromFile:
		path, _ := cmd.Flags().GetString("token-file")
		if path == "" {
			return "", fmt.Errorf("--token-file: path must not be empty")
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read --token-file: %w", err)
		}
		raw = string(buf)
	}
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", fmt.Errorf("asana token is empty")
	}
	return token, nil
}

func asanaConnectionRows(conns []asanaConnection) [][]string {
	rows := make([][]string, 0, len(conns))
	for _, c := range conns {
		rows = append(rows, []string{c.ID, c.AsanaWorkspaceName, c.AsanaWorkspaceGID, c.AsanaUserEmail, c.CreatedAt})
	}
	return rows
}

func runAsanaList(cmd *cobra.Command, _ []string) error {
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var resp asanaListResponse
	if err := client.GetJSON(ctx, "/api/workspaces/"+workspaceID+"/asana/connections", &resp); err != nil {
		return err
	}
	if resp.Connections == nil {
		resp.Connections = []asanaConnection{}
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if !resp.Configured {
		fmt.Fprintln(os.Stderr, "Asana integration is not configured on this server (MULTICA_ASANA_SECRET_KEY unset).")
	}
	if len(resp.Connections) == 0 {
		fmt.Fprintln(os.Stderr, "No Asana connections.")
		return nil
	}
	cli.PrintTable(os.Stdout, []string{"ID", "ASANA WORKSPACE", "GID", "TOKEN USER", "CREATED"}, asanaConnectionRows(resp.Connections))
	return nil
}

func runAsanaConnect(cmd *cobra.Command, _ []string) error {
	token, err := resolveAsanaToken(cmd)
	if err != nil {
		return err
	}
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	body := map[string]any{"access_token": token}
	if gid, _ := cmd.Flags().GetString("asana-workspace"); strings.TrimSpace(gid) != "" {
		body["asana_workspace_gid"] = strings.TrimSpace(gid)
	}

	var conn asanaConnection
	err = client.PostJSON(ctx, "/api/workspaces/"+workspaceID+"/asana/connections", body, &conn)
	if err != nil {
		// The server answers 400 asana_workspace_required with the choices
		// when the token spans several Asana workspaces. Surface them so the
		// user can rerun with --asana-workspace instead of guessing gids.
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) {
			var ambiguous struct {
				Code       string `json:"code"`
				Workspaces []struct {
					GID  string `json:"gid"`
					Name string `json:"name"`
				} `json:"workspaces"`
			}
			if json.Unmarshal([]byte(httpErr.Body), &ambiguous) == nil && ambiguous.Code == "asana_workspace_required" {
				fmt.Fprintln(os.Stderr, "The token can see several Asana workspaces. Rerun with --asana-workspace <gid>:")
				for _, ws := range ambiguous.Workspaces {
					fmt.Fprintf(os.Stderr, "  %s  %s\n", ws.GID, ws.Name)
				}
				return fmt.Errorf("asana workspace is ambiguous")
			}
		}
		return err
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, conn)
	}
	cli.PrintTable(os.Stdout, []string{"ID", "ASANA WORKSPACE", "GID", "TOKEN USER", "CREATED"}, asanaConnectionRows([]asanaConnection{conn}))
	return nil
}

func runAsanaDisconnect(cmd *cobra.Command, args []string) error {
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	if err := client.DeleteJSON(ctx, "/api/workspaces/"+workspaceID+"/asana/connections/"+args[0]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Disconnected Asana connection %s.\n", args[0])
	return nil
}
