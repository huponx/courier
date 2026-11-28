// Package directory resolves identity users to the address and locale courier sends to.
package directory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	identityv1 "github.com/huponx/protogen/go/identity/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type User struct {
	ID     string
	Email  string
	Name   string
	Locale string
}

// Resolver looks users up. A user that does not exist is absent from the result (not an error).
type Resolver interface {
	Users(ctx context.Context, ids []string) (map[string]User, error)
}

type freshKey struct{}

// Fresh makes Users skip the cache for this call: a security mail (a password reset after the address was changed) must
// go to the address of now, not to the one a minute ago.
func Fresh(ctx context.Context) context.Context { return context.WithValue(ctx, freshKey{}, true) }

// Identity resolves through the GetUser RPC of identity, with a short cache: the same admins are
// resolved again for every registration, and a change of e-mail or language is rare.
type Identity struct {
	client identityv1.IdentityServiceClient
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	user    User
	found   bool
	expires time.Time
}

func NewIdentity(c identityv1.IdentityServiceClient, ttl time.Duration) *Identity {
	return &Identity{client: c, ttl: ttl, now: time.Now, cache: map[string]entry{}}
}

func (i *Identity) Users(ctx context.Context, ids []string) (map[string]User, error) {
	out := make(map[string]User, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, done := out[id]; done {
			continue
		}
		e, ok := i.cached(id)
		if ctx.Value(freshKey{}) != nil {
			ok = false
		}
		if !ok {
			resp, err := i.client.GetUser(ctx, &identityv1.GetUserRequest{Id: id})
			switch {
			case status.Code(err) == codes.NotFound:
				e = entry{found: false}
			case err != nil:
				return nil, fmt.Errorf("identity GetUser %s: %w", id, err)
			default:
				u := resp.GetUser()
				// a disabled account gets nothing, as a deleted one gets nothing
				if u.GetStatus() == identityv1.UserStatus_USER_STATUS_DISABLED {
					e = entry{found: false}
					break
				}
				e = entry{found: true, user: User{ID: u.GetId(), Email: u.GetEmail(), Name: u.GetName(), Locale: u.GetLocale()}}
			}
			i.store(id, e)
		}
		if e.found {
			out[id] = e.user
		}
	}
	return out, nil
}

func (i *Identity) cached(id string) (entry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.cache[id]
	if !ok || i.now().After(e.expires) {
		delete(i.cache, id)
		return entry{}, false
	}
	return e, true
}

func (i *Identity) store(id string, e entry) {
	e.expires = i.now().Add(i.ttl)
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.cache) > 10000 { // bounded: drop everything rather than track recency
		i.cache = map[string]entry{}
	}
	i.cache[id] = e
}

// ListUserIDs calls fn with the ids of the active users, a page at a time; role limits it to the holders of that role.
// It is how an announcement finds its audience.
func (i *Identity) ListUserIDs(ctx context.Context, role string, fn func(ids []string) error) error {
	token := ""
	for {
		resp, err := i.client.ListUsers(ctx, &identityv1.ListUsersRequest{
			PageSize: 200, PageToken: token, Role: role, Status: identityv1.UserStatus_USER_STATUS_ACTIVE,
		})
		if err != nil {
			return fmt.Errorf("identity ListUsers: %w", err)
		}
		ids := make([]string, 0, len(resp.GetUsers()))
		for _, u := range resp.GetUsers() {
			ids = append(ids, u.GetId())
		}
		if len(ids) > 0 {
			if err := fn(ids); err != nil {
				return err
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return nil
		}
	}
}
