package iotwireless

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// fuotaDwell is how long, in seconds, a FUOTA task stays In_FuotaSession.
const fuotaDwell = 0.6

// advance takes one step with every multicast session and FUOTA task under way.
func (s *Service) advance(ctx context.Context) {
	var items []*workItem
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		items, err = storage.Records[workItem](tx, indexPrefix)
		return err
	})
	if err != nil {
		return
	}

	for _, item := range items {
		// A failed step is tried again at the next visit.
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error { return step(tx, item, time.Now()) })
	}
}

// step claims one work item in a transaction and moves its resource one status along, or drops the item once its
// resource is gone or finished.
func step(tx storage.WriteTx, item *workItem, now time.Time) error {
	r, found, err := storage.Read[record](tx, item.Scope.Key(serviceName, item.Kind, item.ID))
	if err != nil {
		return err
	}

	if !found {
		tx.Delete(indexKey(item.Scope, item.Kind, item.ID))
		return nil
	}

	var changed, active bool
	if item.Kind == kindMulticast {
		changed, active = stepSession(r, now)
	} else {
		changed, active = stepTask(r, now)
	}

	if !active {
		tx.Delete(indexKey(item.Scope, item.Kind, item.ID))
	}

	if !changed {
		return nil
	}

	return storage.Write(tx, item.Scope.Key(serviceName, r.Kind, r.ID), r)
}

// stepSession sets a group's status Session_Set when its session starts and ends the session after its timeout.
func stepSession(r *record, now time.Time) (changed, active bool) {
	if r.Session == nil {
		return false, false
	}

	start := common.Value(r.Session.SessionStartTime)
	switch {
	case common.Value(r.Multicast.Status) == statusPending && !now.Before(start):
		r.Multicast.Status = new(statusSessionSet)
		return true, true
	case common.Value(r.Multicast.Status) == statusSessionSet && r.Session.SessionTimeout != nil &&
		!now.Before(start.Add(time.Duration(*r.Session.SessionTimeout)*time.Second)):
		r.Session, r.Multicast.Status = nil, new(statusPending)
		return true, false
	}

	return false, true
}

// stepTask moves a started FUOTA task from waiting to its session and from there to done.
func stepTask(r *record, now time.Time) (changed, active bool) {
	switch common.Value(r.Fuota.Status) {
	case statusWaiting:
		if now.Before(common.Value(r.Fuota.LoRaWAN.StartTime)) {
			return false, true
		}

		r.Fuota.Status, r.Entered = new(statusInSession), common.Unix(now)
		return true, true
	case statusInSession:
		if common.Unix(now)-r.Entered < fuotaDwell {
			return false, true
		}

		r.Fuota.Status = new(statusDone)
		return true, false
	}

	return false, false
}
