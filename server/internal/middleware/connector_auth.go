package middleware

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var connectorTokenPattern = regexp.MustCompile(`^mci_[0-9a-f]{40}$`)

type connectorContextKey int

const ctxKeyConnectorCredentialID connectorContextKey = iota

func ConnectorCredentialIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(ctxKeyConnectorCredentialID).(string)
	return value
}

// ConnectorAuth authenticates the dedicated machine credential used by
// connector ingest. It deliberately has no JWT, PAT, or cookie fallback.
func ConnectorAuth(queries *db.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, header := range []string{"X-User-ID", "X-Workspace-ID", "X-Connector-ID", "X-Actor-Source"} {
				r.Header.Del(header)
			}

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				connectorUnauthorized(w)
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if !connectorTokenPattern.MatchString(token) || queries == nil {
				connectorUnauthorized(w)
				return
			}
			credential, err := queries.GetActiveConnectorCredentialByHash(r.Context(), auth.HashToken(token))
			if err != nil {
				connectorUnauthorized(w)
				return
			}

			r.Header.Set("X-Workspace-ID", uuidToString(credential.WorkspaceID))
			r.Header.Set("X-Connector-ID", uuidToString(credential.ConnectorID))
			r.Header.Set("X-Actor-Source", "connector")
			ctx := context.WithValue(r.Context(), ctxKeyConnectorCredentialID, uuidToString(credential.ID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func connectorUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "invalid connector credential")
}
