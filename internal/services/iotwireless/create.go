package iotwireless

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagsOf reads the tags of a create request.
func tagsOf(tags []api.Tag) (map[string]string, error) {
	out, _ := tagging.Map(tags, func(t api.Tag) (key, value *string) { return t.Key, t.Value }, nil, nil)
	if err := tagging.CheckSet(out); err != nil {
		if errors.Is(err, tagging.ErrLimit) {
			return nil, awserror.New("TooManyTagsException", err.Error())
		}

		return nil, invalid("%v", err)
	}

	return out, nil
}

// tagList renders tags as the API's tag list.
func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(key, value string) api.Tag { return api.Tag{Key: new(key), Value: new(value)} })
}

// tokenRecord is what a ClientRequestToken remembers: the resource it made and the request's fingerprint.
type tokenRecord struct {
	ID   string
	Hash string
}

// fingerprint identifies a create request by everything but its ClientRequestToken.
func fingerprint(in any) (string, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return "", err
	}

	var fields map[string]any
	if decodeErr := json.Unmarshal(data, &fields); decodeErr != nil {
		return "", decodeErr
	}

	delete(fields, "ClientRequestToken")
	data, err = json.Marshal(fields)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// replay returns the resource an earlier request with the same ClientRequestToken made, or nil if there is none. The
// same token with other parameters is a conflict. The returned fingerprint goes to remember.
func replay(tx storage.WriteTx, scope request.Scope, kind string, token *string, in any) (*record, string, error) {
	if token == nil {
		return nil, "", nil
	}

	hash, err := fingerprint(in)
	if err != nil {
		return nil, "", err
	}

	t, found, err := storage.Read[tokenRecord](tx, scope.Key(serviceName, kindToken, kind+"/"+*token))
	if err != nil || !found {
		return nil, hash, err
	}

	if t.Hash != hash {
		return nil, "", conflict("The ClientRequestToken %s was used with different parameters", *token)
	}

	r, _, err := storage.Read[record](tx, scope.Key(serviceName, kind, t.ID))
	return r, hash, err
}

// remember records the resource a ClientRequestToken made.
func remember(tx storage.WriteTx, scope request.Scope, token *string, hash string, r *record) error {
	if token == nil {
		return nil
	}

	return storage.Write(tx, scope.Key(serviceName, kindToken, r.Kind+"/"+*token), tokenRecord{ID: r.ID, Hash: hash})
}

// indexOf reads an index entry.
func indexOf(tx storage.ReadTx, scope request.Scope, kind, name string) (string, bool) {
	v, ok := tx.Get(scope.Key(serviceName, kind, name))
	return string(v), ok
}

// claim points an index entry at its owner, unless another owner holds it.
func claim(tx storage.WriteTx, scope request.Scope, kind, name, owner string) error {
	if held, taken := indexOf(tx, scope, kind, name); taken && held != owner {
		return conflict("%s %s is already in use", kind, name)
	}

	tx.Put(scope.Key(serviceName, kind, name), []byte(owner))
	return nil
}

// newRecord makes a record for a new resource with a generated ID.
func newRecord(scope request.Scope, kind, id string, tags map[string]string) *record {
	return &record{Kind: kind, ID: id, Arn: arn(scope, kind, id), Tags: common.CloneTags(tags)}
}
