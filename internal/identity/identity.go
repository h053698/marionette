package identity

import "context"

// User is an OIDC identity whose role was assigned by the server.
type User struct {
	Subject string `json:"subject"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
}

type userKey struct{}

func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func FromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userKey{}).(User)
	return user, ok && user.Subject != ""
}

func IsAdmin(ctx context.Context) bool {
	user, ok := FromContext(ctx)
	return ok && user.Role == "admin"
}
