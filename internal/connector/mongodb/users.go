package mongodb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newUsersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "users",
		Short: "List users of the database (credentials are never returned)",
		Args:  cobra.NoArgs,
		Example: `  muxcat mongodb users
  muxcat mongodb users --db shop --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			dbName, err := resolveDB(cmd, conn)
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := connect(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Disconnect(context.Background()) }()

			var res struct {
				Users bson.A `bson:"users"`
			}
			if err := client.Database(dbName).RunCommand(ctx,
				bson.D{{Key: "usersInfo", Value: 1}}).Decode(&res); err != nil {
				return classifyErr(err, "list users failed")
			}
			rows := make([][]any, 0, len(res.Users))
			for _, raw := range res.Users {
				d, _ := raw.(bson.D)
				rows = append(rows, userRow(d))
			}
			jsonData, err := relaxedJSON(res.Users)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the user list: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"user", "db", "roles"},
				Rows:     rows,
				Message:  fmt.Sprintf("%d users", len(res.Users)),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

// userRow renders a usersInfo document into a table row; roles render as
// comma-separated "role@db".
func userRow(d bson.D) []any {
	roles := ""
	if ra, ok := docLookup(d, "roles").(bson.A); ok {
		parts := make([]string, 0, len(ra))
		for _, r := range ra {
			if rd, ok := r.(bson.D); ok {
				parts = append(parts, fmt.Sprintf("%v@%v", docLookup(rd, "role"), docLookup(rd, "db")))
			}
		}
		roles = strings.Join(parts, ",")
	}
	return []any{docLookup(d, "user"), docLookup(d, "db"), roles}
}

func newRolesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "roles",
		Short: "List roles of the database (with privilege counts)",
		Args:  cobra.NoArgs,
		Example: `  muxcat mongodb roles
  muxcat mongodb roles --db shop --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			dbName, err := resolveDB(cmd, conn)
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := connect(ctx, cfg, conn)
			if err != nil {
				return err
			}
			defer func() { _ = client.Disconnect(context.Background()) }()

			var res struct {
				Roles bson.A `bson:"roles"`
			}
			if err := client.Database(dbName).RunCommand(ctx,
				bson.D{{Key: "rolesInfo", Value: 1}, {Key: "showPrivileges", Value: true}}).Decode(&res); err != nil {
				return classifyErr(err, "list roles failed")
			}
			rows := make([][]any, 0, len(res.Roles))
			for _, raw := range res.Roles {
				d, _ := raw.(bson.D)
				rows = append(rows, roleRow(d))
			}
			jsonData, err := relaxedJSON(res.Roles)
			if err != nil {
				return output.NewError(output.CodeGeneral, "failed to encode the role list: "+err.Error(), "")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"role", "db", "inheritedRoles", "privileges"},
				Rows:     rows,
				Message:  fmt.Sprintf("%d roles", len(res.Roles)),
				JSONData: jsonData,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("db", "", "override the connection's database for this invocation")
	return c
}

// roleRow renders a rolesInfo document into a table row; inheritedRoles
// and privileges render as counts.
func roleRow(d bson.D) []any {
	inherited, privileges := 0, 0
	if a, ok := docLookup(d, "inheritedRoles").(bson.A); ok {
		inherited = len(a)
	}
	if a, ok := docLookup(d, "privileges").(bson.A); ok {
		privileges = len(a)
	}
	return []any{docLookup(d, "role"), docLookup(d, "db"), inherited, privileges}
}
