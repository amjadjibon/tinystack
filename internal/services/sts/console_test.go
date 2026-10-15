//go:build console

package sts

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestConsoleSessionPreservesVerifiedIdentity(t *testing.T) {
	s := New(&memory.Store{}, nil)
	now := time.Now().Truncate(time.Second)
	s.now = func() time.Time { return now }
	for _, p := range []request.Principal{
		{
			Kind:      request.PrincipalRoot,
			Account:   "123456789012",
			ARN:       "arn:aws:iam::123456789012:root",
			PolicyARN: "arn:aws:iam::123456789012:root",
			ID:        "123456789012",
			UserID:    "123456789012",
		},
		{
			Kind:      request.PrincipalUser,
			Account:   "123456789012",
			ARN:       "arn:aws:iam::123456789012:user/alice",
			ID:        "AIDAALICE",
			UserID:    "AIDAALICE",
			PolicyARN: "arn:aws:iam::123456789012:user/alice",
		},
		{
			Kind:      request.PrincipalRole,
			Account:   "123456789012",
			ARN:       "arn:aws:sts::123456789012:assumed-role/Developer/alice",
			PolicyARN: "arn:aws:iam::123456789012:role/Developer",
			Name:      "Developer",
			ID:        "AROAROLE",
			UserID:    "AROAROLE:alice",
		},
	} {
		t.Run(p.Kind, func(t *testing.T) {
			original := request.Credentials{
				Principal: p,
				AccountID: p.Account,
				Federated: p.Kind == request.PrincipalRole,
				Expires:   now.Add(2 * time.Minute),
			}

			issued, err := s.ConsoleSession(context.Background(), &original)
			if err != nil {
				t.Fatal(err)
			}

			var stored session
			err = s.store.View(context.Background(), func(tx storage.ReadTx) error {
				data, ok := tx.Get(sessionPrefix + *issued.AccessKeyId)
				if !ok {
					t.Fatal("session was not stored")
				}

				return json.Unmarshal(data, &stored)
			})
			if err != nil {
				t.Fatal(err)
			}

			got := request.Credentials{
				Principal: stored.principal(), Federated: stored.Federated,
				Expires: time.Unix(stored.Expires, 0), Secret: stored.Secret, Token: stored.Token,
			}

			if got.Principal.ARN != p.ARN || got.Principal.PolicyARN != p.PolicyARN ||
				got.Principal.UserID != p.UserID ||
				got.Principal.Kind != p.Kind ||
				got.Federated != original.Federated ||
				!got.Expires.Equal(original.Expires) ||
				got.Secret != *issued.SecretAccessKey ||
				got.Token != *issued.SessionToken {
				t.Fatalf("credentials=%+v", got)
			}
		})
	}

	issued, err := s.ConsoleSession(context.Background(), &request.Credentials{AccountID: "123456789012"})
	if err != nil || !issued.Expiration.Equal(now.Add(15*time.Minute)) {
		t.Fatal(issued, err)
	}
}
