package iam

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP specifies HMAC-SHA1.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxMFADevicesPerUser = 8
	totpStepSeconds      = 30
	totpEnableWindow     = 2
	totpResyncWindow     = 20
)

var (
	mfaDeviceName = regexp.MustCompile(`^[\w+=,.@-]{1,226}$`)
	totpCodeRegex = regexp.MustCompile(`^\d{6}$`)
)

type virtualMFA struct {
	Tags        map[string]string `json:",omitempty"`
	Name        string
	Path        string
	Seed        string
	UserName    string `json:",omitempty"`
	CreatedAt   int64
	EnableDate  int64 `json:",omitempty"`
	Drift       int64 `json:",omitempty"`
	LastCounter int64 `json:",omitempty"`
}

func mfaARN(account, path, name string) string {
	return arn(account, kindMFA+path+name)
}

func mfaKey(account, path, name string) string {
	return key(account, kindMFA, strings.TrimPrefix(path+name, "/"))
}

func parseMFASerial(account string, serial *string) (path, name string, err error) {
	if err := required("SerialNumber", serial); err != nil {
		return "", "", err
	}

	rest, ok := strings.CutPrefix(*serial, "arn:aws:iam::"+account+":"+kindMFA)
	i := strings.LastIndex(rest, "/")
	if !ok || i < 0 {
		return "", "", noSuchEntity("MFA device %s cannot be found.", *serial)
	}

	path, name = rest[:i+1], rest[i+1:]
	if !rolePath.MatchString(path) || !mfaDeviceName.MatchString(name) {
		return "", "", noSuchEntity("MFA device %s cannot be found.", *serial)
	}

	return path, name, nil
}

func loadVirtualMFA(tx storage.ReadTx, account string, serial *string) (*virtualMFA, string, error) {
	path, name, err := parseMFASerial(account, serial)
	if err != nil {
		return nil, "", err
	}

	mk := mfaKey(account, path, name)
	d, ok, err := storage.Read[virtualMFA](tx, mk)
	if err == nil && !ok {
		err = noSuchEntity("MFA device %s cannot be found.", *serial)
	}

	return d, mk, err
}

func mfaDevicesOf(tx storage.ReadTx, account, name string) ([]*virtualMFA, error) {
	all, err := list[virtualMFA](tx, account, kindMFA)
	if err != nil {
		return nil, err
	}

	var out []*virtualMFA
	for _, d := range all {
		if d.UserName != "" && strings.EqualFold(d.UserName, name) {
			out = append(out, d)
		}
	}

	return out, nil
}

func renameMFADevices(tx storage.WriteTx, account, from, to string) error {
	devices, err := mfaDevicesOf(tx, account, from)
	if err != nil {
		return err
	}

	for _, d := range devices {
		d.UserName = to
		if writeErr := storage.Write(tx, mfaKey(account, d.Path, d.Name), d); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

// totpCode computes the 6-digit RFC 6238 / RFC 4226 TOTP value for counter.
func totpCode(secret []byte, counter int64) string {
	var buf [8]byte
	// #nosec G115 -- Unix time step counters are non-negative.
	binary.BigEndian.PutUint64(buf[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (int64(sum[offset]&0x7f) << 24) |
		(int64(sum[offset+1]) << 16) |
		(int64(sum[offset+2]) << 8) |
		int64(sum[offset+3])
	return fmt.Sprintf("%06d", code%1_000_000)
}

func verifyConsecutiveTOTP(seed string, currentStep int64, code1, code2 string, window int64) (int64, bool) {
	if !totpCodeRegex.MatchString(code1) || !totpCodeRegex.MatchString(code2) {
		return 0, false
	}

	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(seed, "="))
	if err != nil || len(secret) == 0 {
		return 0, false
	}

	for delta := -window; delta <= window; delta++ {
		step := currentStep + delta
		if step < 0 {
			continue
		}

		if totpCode(secret, step) == code1 && totpCode(secret, step+1) == code2 {
			return step, true
		}
	}

	return 0, false
}

func invalidAuthCode() error {
	return awserror.New("InvalidAuthenticationCodeException", "Invalid authentication code.")
}

func (s *Service) CreateVirtualMFADevice(
	ctx context.Context,
	in *api.CreateVirtualMFADeviceRequest,
) (*api.CreateVirtualMFADeviceResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("VirtualMFADeviceName", in.VirtualMFADeviceName, mfaDeviceName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	rawSeed := make([]byte, 20)
	if _, err = rand.Read(rawSeed); err != nil {
		return nil, err
	}

	seed := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rawSeed)
	d := &virtualMFA{
		Tags:      tags,
		Name:      *in.VirtualMFADeviceName,
		Path:      path,
		Seed:      seed,
		CreatedAt: s.now().UnixMilli(),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		mk := mfaKey(m.AccountID, d.Path, d.Name)
		if _, exists := tx.Get(mk); exists {
			return alreadyExists("MFADevice entity at the same path and name already exists.")
		}

		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	out := &api.VirtualMFADevice{
		SerialNumber:     new(mfaARN(m.AccountID, d.Path, d.Name)),
		Base32StringSeed: []byte(d.Seed),
	}
	if len(d.Tags) > 0 {
		out.Tags = tagList(d.Tags)
	}

	return &api.CreateVirtualMFADeviceResponse{VirtualMFADevice: out}, nil
}

func (s *Service) DeleteVirtualMFADevice(
	ctx context.Context,
	in *api.DeleteVirtualMFADeviceRequest,
) (*api.DeleteVirtualMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		if d.UserName != "" {
			return deleteConflict("Cannot delete entity, must deactivate MFA device first.")
		}

		tx.Delete(mk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVirtualMFADeviceOutput{}, nil
}

func (s *Service) EnableMFADevice(
	ctx context.Context,
	in *api.EnableMFADeviceRequest,
) (*api.EnableMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("AuthenticationCode1", in.AuthenticationCode1); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := required("AuthenticationCode2", in.AuthenticationCode2); guardErr != nil {
		return nil, guardErr
	}

	now := s.now()
	currentStep := now.Unix() / totpStepSeconds

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		if d.UserName != "" {
			return alreadyExists("MFA device %s is already enabled for a user.", *in.SerialNumber)
		}

		assigned, listErr := mfaDevicesOf(tx, m.AccountID, u.Name)
		if listErr != nil {
			return listErr
		}

		if len(assigned) >= maxMFADevicesPerUser {
			return limitExceeded("Cannot exceed quota for MFADevicesPerUser: %d", maxMFADevicesPerUser)
		}

		matchedStep, ok := verifyConsecutiveTOTP(
			d.Seed, currentStep, *in.AuthenticationCode1, *in.AuthenticationCode2, totpEnableWindow,
		)
		if !ok {
			return invalidAuthCode()
		}

		d.UserName = u.Name
		d.EnableDate = now.UnixMilli()
		d.Drift = matchedStep - currentStep
		d.LastCounter = matchedStep + 1
		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableMFADeviceOutput{}, nil
}

func (s *Service) DeactivateMFADevice(
	ctx context.Context,
	in *api.DeactivateMFADeviceRequest,
) (*api.DeactivateMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		if !strings.EqualFold(d.UserName, u.Name) {
			return noSuchEntity("MFA device %s is not assigned to user %s.", *in.SerialNumber, u.Name)
		}

		d.UserName = ""
		d.EnableDate = 0
		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeactivateMFADeviceOutput{}, nil
}

func (s *Service) ResyncMFADevice(
	ctx context.Context,
	in *api.ResyncMFADeviceRequest,
) (*api.ResyncMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("AuthenticationCode1", in.AuthenticationCode1); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := required("AuthenticationCode2", in.AuthenticationCode2); guardErr != nil {
		return nil, guardErr
	}

	currentStep := s.now().Unix() / totpStepSeconds

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		if !strings.EqualFold(d.UserName, u.Name) {
			return noSuchEntity("MFA device %s is not assigned to user %s.", *in.SerialNumber, u.Name)
		}

		matchedStep, ok := verifyConsecutiveTOTP(
			d.Seed, currentStep, *in.AuthenticationCode1, *in.AuthenticationCode2, totpResyncWindow,
		)
		if !ok {
			return invalidAuthCode()
		}

		d.Drift = matchedStep - currentStep
		d.LastCounter = matchedStep + 1
		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.ResyncMFADeviceOutput{}, nil
}

func (s *Service) ListMFADevices(
	ctx context.Context,
	in *api.ListMFADevicesRequest,
) (*api.ListMFADevicesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var devices []api.MFADevice
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, userErr := keyUser(tx, &m, in.UserName)
		if userErr != nil {
			return userErr
		}

		assigned, listErr := mfaDevicesOf(tx, m.AccountID, u.Name)
		if listErr != nil {
			return listErr
		}

		for _, d := range assigned {
			devices = append(devices, api.MFADevice{
				UserName:     new(u.Name),
				SerialNumber: new(mfaARN(m.AccountID, d.Path, d.Name)),
				EnableDate:   millis(d.EnableDate),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	serial := func(d api.MFADevice) string { return *d.SerialNumber }
	slices.SortFunc(devices, func(a, b api.MFADevice) int { return strings.Compare(serial(a), serial(b)) })
	selected, truncated, next, err := page(devices, serial, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.MFADevice{}
	}

	return &api.ListMFADevicesResponse{MFADevices: selected, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) ListVirtualMFADevices(
	ctx context.Context,
	in *api.ListVirtualMFADevicesRequest,
) (*api.ListVirtualMFADevicesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	status := common.Value(in.AssignmentStatus)
	if status != "" && status != "Any" && status != "Assigned" && status != "Unassigned" {
		return nil, validation("AssignmentStatus must be Assigned, Unassigned, or Any")
	}

	var devices []api.VirtualMFADevice
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := list[virtualMFA](tx, m.AccountID, kindMFA)
		if listErr != nil {
			return listErr
		}

		for _, d := range all {
			assigned := d.UserName != ""
			if (status == "Assigned" && !assigned) || (status == "Unassigned" && assigned) {
				continue
			}

			item := api.VirtualMFADevice{
				SerialNumber: new(mfaARN(m.AccountID, d.Path, d.Name)),
			}
			if len(d.Tags) > 0 {
				item.Tags = tagList(d.Tags)
			}

			if assigned {
				item.EnableDate = millis(d.EnableDate)
				if u, _, userErr := loadUser(tx, m.AccountID, new(d.UserName)); userErr == nil {
					item.User = userOutput(m.AccountID, u, true)
				}
			}

			devices = append(devices, item)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	serial := func(d api.VirtualMFADevice) string { return *d.SerialNumber }
	slices.SortFunc(devices, func(a, b api.VirtualMFADevice) int { return strings.Compare(serial(a), serial(b)) })
	selected, truncated, next, err := page(devices, serial, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.VirtualMFADevice{}
	}

	return &api.ListVirtualMFADevicesResponse{VirtualMFADevices: selected, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) GetMFADevice(
	ctx context.Context,
	in *api.GetMFADeviceRequest,
) (*api.GetMFADeviceResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.GetMFADeviceResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, _, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		if in.UserName != nil && !strings.EqualFold(d.UserName, *in.UserName) {
			return noSuchEntity("MFA device %s cannot be found for user %s.", *in.SerialNumber, *in.UserName)
		}

		out = &api.GetMFADeviceResponse{SerialNumber: new(mfaARN(m.AccountID, d.Path, d.Name))}
		if d.UserName != "" {
			out.UserName = new(d.UserName)
			out.EnableDate = millis(d.EnableDate)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) TagMFADevice(ctx context.Context, in *api.TagMFADeviceRequest) (*api.TagMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		merged, mergeErr := tagging.Merge(d.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		d.Tags = merged
		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagMFADeviceOutput{}, nil
}

func (s *Service) UntagMFADevice(
	ctx context.Context,
	in *api.UntagMFADeviceRequest,
) (*api.UntagMFADeviceOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, mk, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		d.Tags = tagging.Remove(d.Tags, in.TagKeys)
		return storage.Write(tx, mk, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagMFADeviceOutput{}, nil
}

func (s *Service) ListMFADeviceTags(
	ctx context.Context,
	in *api.ListMFADeviceTagsRequest,
) (*api.ListMFADeviceTagsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListMFADeviceTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, _, loadErr := loadVirtualMFA(tx, m.AccountID, in.SerialNumber)
		if loadErr != nil {
			return loadErr
		}

		out.Tags, out.IsTruncated, out.Marker, loadErr = page(
			tagList(d.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
