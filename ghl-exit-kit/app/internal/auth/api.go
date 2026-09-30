package auth

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// idPath is the input of operations on /{id}. It is used as a whole input,
// never embedded: Huma ignores params of unexported embedded structs.
type idPath struct {
	ID int64 `path:"id" minimum:"1" doc:"ID" example:"1"`
}

type noContent struct{}

// sensitive marks operations whose response carries a one-time secret.
func sensitive() map[string]any { return map[string]any{api.MetaSensitive: true} }

// UserOutput is a single user with its ETag.
type UserOutput struct {
	ETag string `header:"ETag" doc:"Version of the user; send it back in If-Match"`
	Body User
}

func userOutput(u User) *UserOutput { return &UserOutput{ETag: api.ETag(u.Version), Body: u} }

type createUserInput struct {
	Body struct {
		Email    string `json:"email" maxLength:"254" doc:"Login email" example:"editor@example.com"`
		Name     string `json:"name,omitempty" maxLength:"200" doc:"Display name" example:"Sam Editor"`
		Password string `json:"password,omitempty" minLength:"12" maxLength:"256" doc:"At least 12 characters. Omit to generate one, returned once in generated_password."`
	}
}

type createUserOutput struct {
	Body struct {
		User              User   `json:"user"`
		GeneratedPassword string `json:"generated_password,omitempty" doc:"The generated password, shown only in this response"`
	}
}

type updateUserInput struct {
	ID int64 `path:"id" minimum:"1" doc:"User ID" example:"1"`
	api.IfMatchParam
	Body struct {
		Email    *string `json:"email,omitempty" nullable:"false" maxLength:"254" example:"editor@example.com"`
		Name     *string `json:"name,omitempty" nullable:"false" maxLength:"200" example:"Eddie Editor"`
		Password *string `json:"password,omitempty" nullable:"false" minLength:"12" maxLength:"256" doc:"New password; signs the user out everywhere"`
	}
}

type deleteUserInput struct {
	ID int64 `path:"id" minimum:"1" doc:"User ID" example:"1"`
	api.IfMatchParam
}

type resetTOTPInput struct {
	ID int64 `path:"id" minimum:"1" doc:"User ID" example:"1"`
	api.IfMatchParam
}

type totpOutput struct {
	Body TOTPEnrollment
}

type confirmTOTPInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"User ID" example:"1"`
	Body struct {
		Secret string `json:"secret" maxLength:"128" doc:"The secret returned by begin-user-totp"`
		Code   string `json:"code" minLength:"6" maxLength:"8" doc:"The current 6-digit code from the authenticator app" example:"123456"`
	}
}

type listTokensInput struct {
	api.PageParams
	IncludeRevoked bool `query:"include_revoked" doc:"Also list revoked tokens"`
}

type tokenOutput struct {
	Body Token
}

type createTokenInput struct {
	Body struct {
		Name      string     `json:"name" minLength:"1" maxLength:"100" doc:"Unique name, e.g. what uses the token" example:"payment-integration"`
		Scope     api.Scope  `json:"scope" enum:"admin,read" doc:"admin may do everything; read may only call read operations" example:"read"`
		ExpiresAt *time.Time `json:"expires_at,omitempty" doc:"Optional expiry (RFC 3339); absent = never"`
	}
}

type createTokenOutput struct {
	Body struct {
		Token  Token  `json:"token"`
		Secret string `json:"secret" doc:"The token to send as \"Authorization: Bearer ...\". Shown only in this response." example:"mr_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"`
	}
}

// MeActor identifies the caller in GET /me.
type MeActor struct {
	Type string `json:"type" enum:"user,token" example:"token"`
	ID   string `json:"id" example:"3"`
	Name string `json:"name" example:"setup-agent"`
}

type meOutput struct {
	Body struct {
		Actor MeActor   `json:"actor"`
		Scope api.Scope `json:"scope" enum:"admin,read"`
		User  *User     `json:"user,omitempty" doc:"The signed-in user, for session requests"`
		Token *Token    `json:"token,omitempty" doc:"The token in use, for token requests"`
	}
}

// RegisterAPI registers the users, tokens and /me operations.
func (s *Service) RegisterAPI(a *api.API) {
	s.registerUsers(a)
	s.registerTokens(a)

	api.Register(a, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/me",
		Summary:     "Who am I",
		Description: "Returns the caller: the token (name, scope, expiry) or the signed-in user. Use it to check that a token works and what it may do.",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		p, ok := api.PrincipalFromContext(ctx)
		if !ok {
			return nil, api.ErrUnauthorized
		}
		out := &meOutput{}
		out.Body.Actor = MeActor{Type: p.Actor.Type, ID: p.Actor.ID, Name: p.Actor.Name}
		out.Body.Scope = p.Scope
		id, _ := strconv.ParseInt(p.Actor.ID, 10, 64)
		switch p.Actor.Type {
		case core.ActorUser:
			u, err := s.GetUser(ctx, id)
			if err != nil {
				return nil, err
			}
			out.Body.User = &u
		case core.ActorToken:
			t, err := s.GetToken(ctx, id)
			if err != nil {
				return nil, err
			}
			out.Body.Token = &t
		}
		return out, nil
	})
}

func (s *Service) registerUsers(a *api.API) {
	tags := []string{"Users"}
	api.Register(a, huma.Operation{
		OperationID: "list-users", Method: http.MethodGet, Path: "/users", Tags: tags,
		Summary:     "List users",
		Description: "Returns the admin UI users. All users have the same permissions in v1.",
	}, func(ctx context.Context, in *api.PageParams) (*api.ListOutput[User], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		us, err := s.ListUsers(ctx, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[User]{Body: api.NewList(us, limit, func(u User) int64 { return u.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-user", Method: http.MethodPost, Path: "/users", Tags: tags, DefaultStatus: http.StatusCreated,
		Metadata:    sensitive(),
		Summary:     "Create a user",
		Description: "Creates an admin UI user. Passwords are stored as argon2id hashes. Omit the password to have one generated; it is returned once in generated_password.",
	}, func(ctx context.Context, in *createUserInput) (*createUserOutput, error) {
		u, gen, err := s.CreateUser(ctx, CreateUserInput{Email: in.Body.Email, Name: in.Body.Name, Password: in.Body.Password})
		if err != nil {
			return nil, err
		}
		out := &createUserOutput{}
		out.Body.User, out.Body.GeneratedPassword = u, gen
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-user", Method: http.MethodGet, Path: "/users/{id}", Tags: tags,
		Summary:     "Get a user",
		Description: "Returns one user. The ETag header carries its version for If-Match.",
	}, func(ctx context.Context, in *idPath) (*UserOutput, error) {
		u, err := s.GetUser(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return userOutput(u), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-user", Method: http.MethodPatch, Path: "/users/{id}", Tags: tags,
		Summary:     "Update a user",
		Description: "Changes a user's email, name or password; omitted fields stay unchanged. A new password or email signs the user out of every session. Send If-Match to avoid overwriting a concurrent change (412).",
	}, func(ctx context.Context, in *updateUserInput) (*UserOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		u, err := s.UpdateUser(ctx, in.ID, UpdateUserInput{Email: in.Body.Email, Name: in.Body.Name, Password: in.Body.Password}, v)
		if err != nil {
			return nil, err
		}
		return userOutput(u), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-user", Method: http.MethodDelete, Path: "/users/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete a user",
		Description: "Deletes a user, ends their sessions and revokes the API tokens they created (created_by `user:<id>`), so a departing user's token stops working too. The last remaining user can't be deleted (409).",
	}, func(ctx context.Context, in *deleteUserInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		return &noContent{}, s.DeleteUser(ctx, in.ID, v)
	})

	api.Register(a, huma.Operation{
		OperationID: "begin-user-totp", Method: http.MethodPost, Path: "/users/{id}/totp", Tags: tags,
		Metadata:    sensitive(),
		Summary:     "Start TOTP enrolment",
		Description: "Generates a new TOTP secret and otpauth:// URI for the user's authenticator app. Nothing changes until confirm-user-totp is called with a valid code for this secret.",
	}, func(ctx context.Context, in *idPath) (*totpOutput, error) {
		e, err := s.BeginTOTP(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &totpOutput{Body: e}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "confirm-user-totp", Method: http.MethodPost, Path: "/users/{id}/totp/confirm", Tags: tags,
		Summary:     "Turn TOTP on",
		Description: "Activates two-factor login for the user with the secret from begin-user-totp, once the given code is valid (30-second codes, one step of clock drift allowed). The user is signed out everywhere and logs in again with a code.",
	}, func(ctx context.Context, in *confirmTOTPInput) (*UserOutput, error) {
		u, err := s.ConfirmTOTP(ctx, in.ID, in.Body.Secret, in.Body.Code)
		if err != nil {
			return nil, err
		}
		return userOutput(u), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "reset-user-totp", Method: http.MethodDelete, Path: "/users/{id}/totp", Tags: tags,
		Summary:     "Reset TOTP",
		Description: "Turns two-factor login off for the user, e.g. after a lost phone, and signs them out everywhere. They can enrol again afterwards. Send If-Match to avoid acting on a user that changed since you read it (412).",
	}, func(ctx context.Context, in *resetTOTPInput) (*UserOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		u, err := s.ResetTOTP(ctx, in.ID, v)
		if err != nil {
			return nil, err
		}
		return userOutput(u), nil
	})
}

func (s *Service) registerTokens(a *api.API) {
	tags := []string{"API tokens"}
	api.Register(a, huma.Operation{
		OperationID: "list-tokens", Method: http.MethodGet, Path: "/tokens", Tags: tags,
		Summary:     "List API tokens",
		Description: "Returns API tokens with their name, scope, prefix, expiry and last use. Secrets are never shown again after creation. Revoked tokens are hidden unless include_revoked=true.",
	}, func(ctx context.Context, in *listTokensInput) (*api.ListOutput[Token], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		ts, err := s.ListTokens(ctx, after, limit+1, in.IncludeRevoked)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Token]{Body: api.NewList(ts, limit, func(t Token) int64 { return t.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-token", Method: http.MethodPost, Path: "/tokens", Tags: tags, DefaultStatus: http.StatusCreated,
		Metadata:    sensitive(),
		Summary:     "Create an API token",
		Description: "Creates an API token and returns its secret once; store it safely. Scope admin may do everything, scope read only read operations. Tokens are stored hashed, can expire and can be revoked.",
	}, func(ctx context.Context, in *createTokenInput) (*createTokenOutput, error) {
		t, secret, err := s.CreateToken(ctx, TokenInput{Name: in.Body.Name, Scope: in.Body.Scope, ExpiresAt: in.Body.ExpiresAt})
		if err != nil {
			return nil, err
		}
		out := &createTokenOutput{}
		out.Body.Token, out.Body.Secret = t, secret
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-token", Method: http.MethodGet, Path: "/tokens/{id}", Tags: tags,
		Summary:     "Get an API token",
		Description: "Returns one API token (without its secret).",
	}, func(ctx context.Context, in *idPath) (*tokenOutput, error) {
		t, err := s.GetToken(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &tokenOutput{Body: t}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "revoke-token", Method: http.MethodDelete, Path: "/tokens/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Revoke an API token",
		Description: "Revokes an API token; it stops working immediately. The record stays for the audit trail, and its name can't be reused.",
	}, func(ctx context.Context, in *idPath) (*noContent, error) {
		return &noContent{}, s.RevokeToken(ctx, in.ID)
	})
}
