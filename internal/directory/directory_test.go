package directory

import (
	"context"
	"testing"
	"time"

	identityv1 "github.com/huponx/protogen/go/identity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeIdentity struct {
	identityv1.IdentityServiceClient
	calls int
}

func (f *fakeIdentity) GetUser(_ context.Context, req *identityv1.GetUserRequest, _ ...grpc.CallOption) (*identityv1.GetUserResponse, error) {
	f.calls++
	if req.GetId() == "gone" {
		return nil, status.Error(codes.NotFound, "no user")
	}
	if req.GetId() == "off" {
		return &identityv1.GetUserResponse{User: &identityv1.User{Id: "off", Email: "off@x.io", Status: identityv1.UserStatus_USER_STATUS_DISABLED}}, nil
	}
	if req.GetId() == "boom" {
		return nil, status.Error(codes.Unavailable, "down")
	}
	return &identityv1.GetUserResponse{User: &identityv1.User{Id: req.GetId(), Email: req.GetId() + "@x.io", Name: "N", Locale: "vi"}}, nil
}

func TestUsersCachesAndSkipsMissing(t *testing.T) {
	f := &fakeIdentity{}
	d := NewIdentity(f, time.Minute)
	got, err := d.Users(context.Background(), []string{"a", "gone", "a", " "})
	if err != nil || len(got) != 1 || got["a"].Email != "a@x.io" || got["a"].Locale != "vi" {
		t.Fatalf("%v %v", got, err)
	}
	if f.calls != 2 {
		t.Fatalf("duplicate id must be one call: %d", f.calls)
	}
	_, _ = d.Users(context.Background(), []string{"a", "gone"})
	if f.calls != 2 {
		t.Fatalf("second round must hit the cache (also for missing users): %d", f.calls)
	}
	now := time.Now().Add(2 * time.Minute)
	d.now = func() time.Time { return now }
	_, _ = d.Users(context.Background(), []string{"a"})
	if f.calls != 3 {
		t.Fatalf("expired entry must be fetched again: %d", f.calls)
	}
}

func TestUsersPropagatesOutage(t *testing.T) {
	d := NewIdentity(&fakeIdentity{}, time.Minute)
	if _, err := d.Users(context.Background(), []string{"boom"}); err == nil {
		t.Fatal("an identity outage must be an error, not a silent skip")
	}
}

func TestDisabledUserIsAsGoneAsADeletedOne(t *testing.T) {
	d := NewIdentity(&fakeIdentity{}, time.Minute)
	got, err := d.Users(context.Background(), []string{"a", "off"})
	if err != nil || len(got) != 1 || got["a"].ID != "a" {
		t.Fatalf("%v %v", got, err)
	}
}

// a security mail goes to the address of now: Fresh skips the cache
func TestFreshSkipsTheCache(t *testing.T) {
	f := &fakeIdentity{}
	d := NewIdentity(f, time.Hour)
	_, _ = d.Users(context.Background(), []string{"a"})
	_, _ = d.Users(context.Background(), []string{"a"})
	if f.calls != 1 {
		t.Fatalf("cached: %d", f.calls)
	}
	_, _ = d.Users(Fresh(context.Background()), []string{"a"})
	if f.calls != 2 {
		t.Fatalf("fresh must call identity again: %d", f.calls)
	}
}
