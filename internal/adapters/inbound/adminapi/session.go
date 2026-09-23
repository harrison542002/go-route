package adminapi

import (
	"context"
	"errors"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// Login exchanges an email and a password for a session. Every way it can fail
// answers with this one sentence, so the response says nothing about whether
// the email exists, the password was close, or the account is disabled.
func (h *Handler) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	session, err := h.auth.Login(ctx, ports.Credentials{
		Email:    string(req.Body.Email),
		Password: req.Body.Password,
		ClientIP: clientIPFrom(ctx),
	})
	if errors.Is(err, ports.ErrUnauthenticated) {
		return gen.Login401JSONResponse{UnauthorizedJSONResponse: unauthorized("invalid email or password")}, nil
	}
	if err != nil {
		return nil, err
	}
	return gen.Login200JSONResponse(toSession(session)), nil
}

func (h *Handler) RefreshSession(
	ctx context.Context, req gen.RefreshSessionRequestObject,
) (gen.RefreshSessionResponseObject, error) {
	session, err := h.auth.Refresh(ctx, req.Body.RefreshToken)
	if errors.Is(err, ports.ErrUnauthenticated) {
		return gen.RefreshSession401JSONResponse{
			UnauthorizedJSONResponse: unauthorized("invalid refresh token"),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return gen.RefreshSession200JSONResponse(toSession(session)), nil
}

// Logout answers 204 whether or not the token was live, because the caller
// asked for it to stop working and it has.
func (h *Handler) Logout(ctx context.Context, req gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	if err := h.auth.Logout(ctx, actorFrom(ctx), req.Body.RefreshToken); err != nil {
		return nil, err
	}
	return gen.Logout204Response{}, nil
}

// Whoami answers for both credential kinds. A machine credential has no
// account behind it, so it gets its actor and its role and no user.
func (h *Handler) Whoami(ctx context.Context, _ gen.WhoamiRequestObject) (gen.WhoamiResponseObject, error) {
	id := identityFrom(ctx)
	out := gen.Identity{
		Actor: id.Actor,
		Kind:  gen.IdentityKind(id.Kind),
		Role:  gen.AdminRole(id.Role),
	}
	if id.User != nil {
		user := toAdminUser(*id.User)
		out.User = &user
	}
	return gen.Whoami200JSONResponse(out), nil
}

func unauthorized(message string) gen.UnauthorizedJSONResponse {
	scheme := "Bearer"
	return gen.UnauthorizedJSONResponse{
		Body: gen.Error{Error: gen.ErrorBody{Message: message, Type: gen.AuthenticationError}},
		Headers: gen.UnauthorizedResponseHeaders{
			WWWAuthenticate: &scheme,
		},
	}
}

func toSession(s ports.AdminSessionTokens) gen.Session {
	return gen.Session{
		AccessToken:  s.AccessToken,
		TokenType:    gen.Bearer,
		ExpiresAt:    s.ExpiresAt,
		RefreshToken: s.RefreshToken,
		User:         toAdminUser(s.User),
	}
}

func toAdminUser(u domains.AdminUser) gen.AdminUser {
	return gen.AdminUser{
		Id:          u.ID,
		Email:       openapi_types.Email(u.Email),
		Role:        gen.AdminRole(u.Role),
		CreatedAt:   u.CreatedAt,
		DisabledAt:  nullableOf(u.DisabledAt),
		LastLoginAt: nullableOf(u.LastLoginAt),
	}
}
