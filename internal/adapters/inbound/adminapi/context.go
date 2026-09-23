package adminapi

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// Everything a handler needs besides its decoded input is put into the request
// context by the middleware in server.go, and read back here.

type contextKey int

const (
	identityKey contextKey = iota
	clientIPKey
)

// identityKind is which of the two credential kinds authenticated. Nothing
// past the middleware branches on it except /auth/me, which reports it.
type identityKind string

const (
	kindUser    identityKind = "user"
	kindMachine identityKind = "machine"
)

// identity is all the rest of the chain may know about the caller: the name
// writes are audited under, the role the permission check reads, and which
// kind of credential said so. For a person the role is the one on their row,
// read this request -- never the one copied into their access token, which
// cannot change when the row does.
type identity struct {
	Kind  identityKind
	Actor string
	Role  domains.Role

	// User is set for a person and nil for a machine credential, which has
	// no account behind it.
	User *domains.AdminUser
}

func machineIdentity(cred domains.AdminCredential) identity {
	return identity{Kind: kindMachine, Actor: cred.Actor(), Role: cred.Role}
}

func userIdentity(user domains.AdminUser) identity {
	return identity{Kind: kindUser, Actor: user.Actor(), Role: user.Role, User: &user}
}

func withIdentity(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// unknownActor is recorded by a handler reached without RequireToken, rather
// than claiming a credential that never authenticated.
const unknownActor = "admin:unknown"

// identityFrom reports who authenticated. The zero identity allows nothing and
// is attributed to nobody, so a check reached without authentication refuses.
func identityFrom(ctx context.Context) identity {
	id, _ := ctx.Value(identityKey).(identity)
	return id
}

func actorFrom(ctx context.Context) string {
	if actor := identityFrom(ctx).Actor; actor != "" {
		return actor
	}
	return unknownActor
}

func roleFrom(ctx context.Context) domains.Role { return identityFrom(ctx).Role }

func withClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// clientIPFrom reports the address the request arrived from, empty when it
// could not be read. It is what the login throttle counts against; no proxy
// header is trusted, because anyone may send one.
func clientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

func writeFrom(ctx context.Context) ports.Write {
	return ports.Write{Actor: actorFrom(ctx)}
}
