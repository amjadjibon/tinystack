package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"net/http"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

const trailerSignature = "x-amz-trailer-signature"

type signedChunks struct {
	advertised  map[string]bool
	verifier    *sigv4.ChunkVerifier
	digest      hash.Hash
	signature   string
	trailerSig  string
	withTrailer bool
}

func signedChunkOptions(r *http.Request, verifier *sigv4.ChunkVerifier) (*signedChunks, error) {
	hashMode := r.Header.Get("X-Amz-Content-Sha256")
	if hashMode != sigv4.StreamingPayload && hashMode != sigv4.StreamingPayloadTrailer {
		return nil, nil //nolint:nilnil // Ordinary uploads have no signed chunk options.
	}

	if len(r.Trailer) != 0 || r.Header.Get("X-Amz-Decoded-Content-Length") == "" {
		return nil, invalid("Signed aws-chunked uploads require decoded content length and inline trailers")
	}

	s := &signedChunks{verifier: verifier, withTrailer: hashMode == sigv4.StreamingPayloadTrailer}
	if verifier != nil {
		s.digest = sha256.New()
	}

	s.advertised = make(map[string]bool)
	for _, field := range r.Header.Values("X-Amz-Trailer") {
		for _, part := range strings.Split(field, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" || name == trailerSignature || s.advertised[name] {
				return nil, invalid("Invalid x-amz-trailer header")
			}

			s.advertised[name] = true
		}
	}

	if s.withTrailer != (len(s.advertised) > 0) {
		return nil, invalid("x-amz-trailer must match the streaming payload type")
	}

	return s, nil
}

func validChunkSignature(value string) bool {
	if len(value) != 2*sha256.Size {
		return false
	}

	var buf [sha256.Size]byte
	_, err := hex.Decode(buf[:], []byte(value))
	return err == nil
}

func chunkSignatureMismatch() error {
	return &awserror.Error{
		Code: "SignatureDoesNotMatch", Message: "The streaming payload signature does not match.",
		StatusCode: http.StatusForbidden,
	}
}

func (c *chunkReader) line(limit int) (string, error) {
	if c.signed == nil {
		return readBoundedLine(c.r, limit)
	}

	line, err := c.r.ReadSlice('\n')
	if err != nil {
		return "", err
	}

	if len(line) < 2 || len(line)-2 > limit || line[len(line)-2] != '\r' {
		return "", errFramingTooLarge
	}

	return string(line[:len(line)-2]), nil
}

func (c *chunkReader) startChunk(extension string) error {
	s := c.signed
	if s == nil {
		return nil
	}

	signature, ok := strings.CutPrefix(extension, "chunk-signature=")
	if !ok || !validChunkSignature(signature) {
		return c.malformed(nil)
	}

	s.signature = signature
	if s.digest != nil {
		s.digest.Reset()
	}

	return nil
}

func (c *chunkReader) finishChunk() error {
	s := c.signed
	if s != nil && s.verifier != nil && !s.verifier.Chunk(s.digest.Sum(nil), s.signature) {
		return chunkSignatureMismatch()
	}

	return nil
}

func (c *chunkReader) trailer(name, value string) error {
	s := c.signed
	if s != nil {
		lower := strings.ToLower(name)
		switch {
		case !s.withTrailer || s.trailerSig != "":
			return c.malformed(nil)
		case lower == trailerSignature:
			if !validChunkSignature(value) {
				return c.malformed(nil)
			}

			s.trailerSig = value
			return nil
		case !s.advertised[lower] || len(c.header.Values(name)) != 0:
			return c.malformed(nil)
		}
	}

	if c.header != nil {
		c.header.Set(name, value)
	}

	return nil
}

func (c *chunkReader) finishTrailers() error {
	s := c.signed
	if s == nil || !s.withTrailer {
		return nil
	}

	if s.trailerSig == "" || len(c.header) != len(s.advertised) {
		return c.malformed(nil)
	}

	if s.verifier != nil {
		var names []string
		for name := range s.advertised {
			names = append(names, name)
		}

		slices.Sort(names)
		var canonical strings.Builder
		for _, name := range names {
			canonical.WriteString(name + ":" + strings.Join(strings.Fields(c.header.Get(name)), " ") + "\n")
		}

		if !s.verifier.Trailer(canonical.String(), s.trailerSig) {
			return chunkSignatureMismatch()
		}
	}

	return nil
}
