// Package iotwireless stores AWS IoT Wireless LoRaWAN configuration and routes simulated uplinks through IoT Core.
//
// Records are JSON under <account>/<region>/iotwireless/<kind>/<id>, with these indexes beside them:
//
//	<scope>/iotwireless/deveui/<DevEui>            the wireless device with that DevEui
//	<scope>/iotwireless/gatewayeui/<GatewayEui>    the wireless gateway with that GatewayEui
//	<scope>/iotwireless/thing/<name>               the device or gateway associated with an IoT thing
//	<scope>/iotwireless/certificate/<id>           the gateway associated with an IoT certificate
//	<scope>/iotwireless/token/<kind>/<token>       the resource a ClientRequestToken made
//	<scope>/iotwireless/message/<device>/<seq>     a device's queued downlink messages
//	<scope>/iotwireless/groupmessage/<group>/<seq> a multicast group's queued downlink messages
//	<scope>/iotwireless/loglevel/...               log levels, stored only
//	aws/iotwireless/index/<scope>/<kind>/<id>      multicast sessions and FUOTA tasks the worker advances
package iotwireless

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/iot"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName        = "iotwireless"
	kindDestination    = "Destination"
	kindDeviceProfile  = "DeviceProfile"
	kindServiceProfile = "ServiceProfile"
	kindDevice         = "WirelessDevice"
	kindGateway        = "WirelessGateway"
	kindMulticast      = "MulticastGroup"
	kindFuota          = "FuotaTask"
	kindDevEui         = "deveui"
	kindGatewayEui     = "gatewayeui"
	kindThing          = "thing"
	kindCertificate    = "certificate"
	kindToken          = "token"
	kindMessage        = "message"
	kindGroupMessage   = "groupmessage"
	kindResourceLog    = "resourcelog"
	kindLogLevels      = "loglevels"
	indexPrefix        = "aws/iotwireless/index/"
	typeLoRaWAN        = "LoRaWAN"
	// maxPage is the page size of a list that sets no MaxResults, the most the model allows.
	maxPage = 250
)

// resourceKinds are the kinds of record that have an ARN and tags.
var resourceKinds = []string{
	kindDestination, kindDeviceProfile, kindServiceProfile, kindDevice, kindGateway, kindMulticast, kindFuota,
}

// Service owns the regional IoT Wireless records and advances multicast sessions and FUOTA tasks.
type Service struct {
	store   storage.Store
	roles   *iam.Service
	things  *iot.Service
	caller  *catalog.Caller
	workers *background.Group
}

// New constructs a service; UseCaller starts the worker.
func New(store storage.Store, roles *iam.Service, things *iot.Service) *Service {
	return &Service{store: store, roles: roles, things: things, workers: background.New()}
}

// UseCaller connects the S3 check of FUOTA firmware images and starts the worker.
func (s *Service) UseCaller(c *catalog.Caller) {
	s.caller = c
	s.workers.Every(200*time.Millisecond, s.advance)
}

// Stop joins the worker before storage closes.
func (s *Service) Stop() { s.workers.Stop() }

// record is one resource of any kind; the member for its Kind holds its configuration.
type record struct {
	Tags           map[string]string
	Destination    *api.GetDestinationResponse
	DeviceProfile  *api.GetDeviceProfileResponse
	ServiceProfile *api.GetServiceProfileResponse
	Device         *api.GetWirelessDeviceResponse
	Gateway        *api.GetWirelessGatewayResponse
	Multicast      *api.GetMulticastGroupResponse
	Fuota          *api.GetFuotaTaskResponse
	// Uplink is the metadata of a device's last simulated uplink, and LastUplink its time, as is a gateway's.
	Uplink *api.LoRaWANDeviceMetadata
	// Session is the multicast session of a multicast group.
	Session    *api.LoRaWANMulticastSession
	LastUplink string
	// Certificate is the IoT certificate a gateway is associated with.
	Certificate string
	Kind        string
	ID          string
	Name        string
	Arn         string
	// Devices and Groups hold the task and multicast membership.
	Devices []string
	Groups  []string
	// Entered is when a FUOTA task entered In_FuotaSession, in Unix seconds.
	Entered float64
}

// workItem is the worker's index entry for a multicast group with a session or a FUOTA task under way.
type workItem struct {
	Scope request.Scope
	Kind  string
	ID    string
}

func invalid(format string, args ...any) error { return awserror.Validation(format, args...) }

func conflict(format string, args ...any) error {
	return awserror.New("ConflictException", fmt.Sprintf(format, args...))
}

func missing(kind, id string) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf("%s %s does not exist", kind, id))
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:iotwireless:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

// iso formats a time as the ISO 8601 text of the API's ISODateTimeString.
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// pageSize is the page size a MaxResults asks for; 0 and absent both mean the default.
func pageSize(requested *int64) int {
	if n := common.Value(requested); n > 0 {
		return int(n)
	}

	return maxPage
}

func badToken() error { return invalid("NextToken is not valid") }

// load reads a record that must exist.
func load(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, missing(kind, id))
	}

	return r, nil
}

func indexKey(scope request.Scope, kind, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + id
}

// update runs fn in a transaction scoped to the request's account and Region.
func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

// view runs fn in a read transaction scoped to the request's account and Region.
func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// checkRole fails unless arn names an IAM role of the request's account.
func (s *Service) checkRole(ctx context.Context, scope request.Scope, roleARN string) error {
	_, found, err := s.roles.RoleByARN(ctx, roleARN)
	if err != nil {
		return err
	}

	if !found || !strings.HasPrefix(roleARN, "arn:aws:iam::"+scope.AccountID+":role/") {
		return invalid("The IAM role %s does not exist in this account", roleARN)
	}

	return nil
}

var _ api.API = (*Service)(nil)
