package eventbridge

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Archives keep the events of a bus that match their pattern (every event,
// without one). An archive is "<account>/<region>/eventbridge/archive/<name>",
// and each event it keeps is "…/eventbridge/archived/<name>/<zero-padded event
// time in ms><event ID>", so a replay reads a time range in order.

const archiveEnabled = "ENABLED"

var archiveName = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,48}$`)

// archive is an archive's record. Times are Unix milliseconds.
type archive struct {
	Name        string
	Bus         string
	Description string `json:",omitempty"`
	Pattern     string `json:",omitempty"`
	Retention   int64
	Created     int64
	Events      int64
	Size        int64
}

func archivePrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/archive/"
}

func archivedPrefix(scope request.Scope, name string) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/archived/" + name + "/"
}

// archivedKey orders an archive's events by their time.
func archivedKey(scope request.Scope, name string, at time.Time, id string) string {
	return archivedPrefix(scope, name) + fmt.Sprintf("%020d", at.UnixMilli()) + id
}

func archiveARN(scope request.Scope, name string) string {
	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":archive/" + name
}

func archiveNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Archive "+name+" does not exist.")
}

func loadArchive(tx storage.ReadTx, scope request.Scope, name string) (*archive, error) {
	a, found, err := storage.Read[archive](tx, archivePrefix(scope)+name)
	if err == nil && !found {
		return nil, archiveNotFound(name)
	}

	return a, err
}

func checkArchive(description, eventPattern *string, retention *int64) error {
	if len(common.Value(description)) > 512 {
		return invalid("Description must be at most 512 characters.")
	}

	if common.Value(retention) < 0 {
		return invalid("RetentionDays must not be negative.")
	}

	if eventPattern != nil {
		if _, err := compilePattern(*eventPattern); err != nil {
			return err
		}
	}

	return nil
}

func (a *archive) state() (state, reason *string) {
	return new(archiveEnabled), new("")
}

func (s *Service) CreateArchive(ctx context.Context, in *api.CreateArchiveRequest) (*api.CreateArchiveResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.ArchiveName)
	if !archiveName.MatchString(name) {
		return nil, invalid("Archive name must be 1 to 48 characters from [.-_A-Za-z0-9].")
	}

	if checkErr := checkArchive(in.Description, in.EventPattern, in.RetentionDays); checkErr != nil {
		return nil, checkErr
	}

	bus, ok := strings.CutPrefix(common.Value(in.EventSourceArn), busARN(m.Scope, ""))
	if !ok {
		return nil, busNotFound(common.Value(in.EventSourceArn))
	}

	a := &archive{
		Name: name, Bus: bus, Description: common.Value(in.Description), Pattern: common.Value(in.EventPattern),
		Retention: common.Value(in.RetentionDays), Created: s.now().UnixMilli(),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if busErr := checkBus(tx, m.Scope, bus); busErr != nil {
			return busErr
		}

		if _, exists := tx.Get(archivePrefix(m.Scope) + name); exists {
			return awserror.New("ResourceAlreadyExistsException", "Archive "+name+" already exists.")
		}

		return storage.Write(tx, archivePrefix(m.Scope)+name, a)
	})
	if err != nil {
		return nil, err
	}

	state, reason := a.state()
	return &api.CreateArchiveResponse{
		ArchiveArn: new(archiveARN(m.Scope, name)), State: state, StateReason: reason,
		CreationTime: new(float64(a.Created) / 1000),
	}, nil
}

func (s *Service) DescribeArchive(
	ctx context.Context,
	in *api.DescribeArchiveRequest,
) (*api.DescribeArchiveResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var a *archive
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		a, loadErr = loadArchive(tx, m.Scope, common.Value(in.ArchiveName))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	l := a.list(m.Scope)
	out := &api.DescribeArchiveResponse{
		ArchiveArn: new(archiveARN(m.Scope, a.Name)), ArchiveName: l.ArchiveName, EventSourceArn: l.EventSourceArn,
		State: l.State, StateReason: l.StateReason, RetentionDays: l.RetentionDays, SizeBytes: l.SizeBytes,
		EventCount: l.EventCount, CreationTime: l.CreationTime,
	}
	if a.Description != "" {
		out.Description = new(a.Description)
	}

	if a.Pattern != "" {
		out.EventPattern = new(a.Pattern)
	}

	return out, nil
}

// list renders an archive as ListArchives does.
func (a *archive) list(scope request.Scope) api.Archive {
	state, reason := a.state()
	return api.Archive{
		ArchiveName: new(a.Name), EventSourceArn: new(busARN(scope, a.Bus)), State: state, StateReason: reason,
		RetentionDays: new(a.Retention), SizeBytes: new(a.Size), EventCount: new(a.Events),
		CreationTime: new(float64(a.Created) / 1000),
	}
}

// ListArchives lists the caller's archives in name order, by name prefix,
// bus, or state.
func (s *Service) ListArchives(ctx context.Context, in *api.ListArchivesRequest) (*api.ListArchivesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.ListArchivesResponse{Archives: []api.Archive{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(archivePrefix(m.Scope) + common.Value(in.NamePrefix)) {
			a, _, loadErr := storage.Read[archive](tx, k)
			if loadErr != nil {
				return loadErr
			}

			l := a.list(m.Scope)
			switch {
			case a.Name <= after:
			case in.EventSourceArn != nil && *in.EventSourceArn != *l.EventSourceArn:
			case in.State != nil && *in.State != *l.State:
			case len(out.Archives) == size:
				out.NextToken = nextToken(*out.Archives[size-1].ArchiveName)
				return nil
			default:
				out.Archives = append(out.Archives, l)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateArchive changes an archive's description, pattern, or retention.
func (s *Service) UpdateArchive(ctx context.Context, in *api.UpdateArchiveRequest) (*api.UpdateArchiveResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkArchive(in.Description, in.EventPattern, in.RetentionDays); checkErr != nil {
		return nil, checkErr
	}

	var a *archive
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if a, loadErr = loadArchive(tx, m.Scope, common.Value(in.ArchiveName)); loadErr != nil {
			return loadErr
		}

		if in.Description != nil {
			a.Description = *in.Description
		}

		if in.EventPattern != nil {
			a.Pattern = *in.EventPattern
		}

		if in.RetentionDays != nil {
			a.Retention = *in.RetentionDays
		}

		return storage.Write(tx, archivePrefix(m.Scope)+a.Name, a)
	})
	if err != nil {
		return nil, err
	}

	state, reason := a.state()
	return &api.UpdateArchiveResponse{
		ArchiveArn: new(archiveARN(m.Scope, a.Name)), State: state, StateReason: reason,
		CreationTime: new(float64(a.Created) / 1000),
	}, nil
}

// DeleteArchive deletes an archive and the events it keeps.
func (s *Service) DeleteArchive(ctx context.Context, in *api.DeleteArchiveRequest) (*api.DeleteArchiveResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadArchive(tx, m.Scope, common.Value(in.ArchiveName))
		if loadErr != nil {
			return loadErr
		}

		for _, k := range tx.Keys(archivedPrefix(m.Scope, a.Name)) {
			tx.Delete(k)
		}

		tx.Delete(archivePrefix(m.Scope) + a.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteArchiveResponse{}, nil
}

// archiver is an archive PutEvents matches events against.
type archiver struct {
	pattern *pattern.Pattern
	name    string
}

// archivers loads the archives of each bus, by bus name.
func (s *Service) archivers(ctx context.Context, scope request.Scope) (map[string][]archiver, error) {
	out := map[string][]archiver{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(archivePrefix(scope)) {
			a, _, err := storage.Read[archive](tx, k)
			if err != nil {
				return err
			}

			var p *pattern.Pattern
			if a.Pattern != "" {
				if p, err = compilePattern(a.Pattern); err != nil {
					return err
				}
			}

			out[a.Bus] = append(out[a.Bus], archiver{pattern: p, name: a.Name})
		}

		return nil
	})

	return out, err
}

// archived is an event an archive keeps.
type archived struct {
	at      time.Time
	archive string
	id      string
	encoded []byte
}

// keep stores events in their archives, counts them, and drops the events
// past each archive's retention.
func (s *Service) keep(ctx context.Context, scope request.Scope, events []archived) error {
	if len(events) == 0 {
		return nil
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		archives := map[string]*archive{}
		for _, e := range events {
			a, ok := archives[e.archive]
			if !ok {
				var found bool
				var err error
				if a, found, err = storage.Read[archive](tx, archivePrefix(scope)+e.archive); err != nil {
					return err
				} else if !found {
					// The archive was deleted after PutEvents read it.
					continue
				}

				archives[e.archive] = a
			}

			tx.Put(archivedKey(scope, a.Name, e.at, e.id), e.encoded)
			a.Events++
			a.Size += int64(len(e.encoded))
		}

		for _, a := range archives {
			if a.Retention > 0 {
				oldest := archivedKey(scope, a.Name, s.now().AddDate(0, 0, -int(a.Retention)), "")
				for _, k := range tx.Keys(archivedPrefix(scope, a.Name)) {
					if k >= oldest {
						break
					}

					data, _ := tx.Get(k)
					a.Events--
					a.Size -= int64(len(data))
					tx.Delete(k)
				}
			}

			if err := storage.Write(tx, archivePrefix(scope)+a.Name, a); err != nil {
				return err
			}
		}

		return nil
	})
}
