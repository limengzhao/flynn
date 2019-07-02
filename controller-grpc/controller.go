//go:generate protoc -I/usr/local/include -I./protobuf -I${GOPATH}/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis --go_out=plugins=grpc:./protobuf ./protobuf/controller.proto
package main

import (
	"encoding/json"
	"errors"
	fmt "fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/flynn/flynn/controller-grpc/protobuf"
	"github.com/flynn/flynn/controller-grpc/utils"
	"github.com/flynn/flynn/controller/data"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pkg/cors"
	"github.com/flynn/flynn/pkg/ctxhelper"
	"github.com/flynn/flynn/pkg/httphelper"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/shutdown"
	routerc "github.com/flynn/flynn/router/client"
	que "github.com/flynn/que-go"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	log "github.com/inconshreveable/log15"
	"github.com/soheilhy/cmux"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/stats"
)

func mustEnv(key string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	panic(fmt.Errorf("%s is required", key))
}

var logger = log.New("component", "controller-grpc")

func main() {
	// Increase resources limitations
	// See https://github.com/eranyanay/1m-go-websockets/blob/master/2_ws_ulimit/server.go
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		panic(err)
	}
	rLimit.Cur = rLimit.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		panic(err)
	}

	logger.Debug("opening database connection...")

	// Open connection to main controller database
	db := postgres.Wait(nil, data.PrepareStatements)
	shutdown.BeforeExit(func() { db.Close() })
	q := que.NewClient(db.ConnPool)

	logger.Debug("initializing server...")

	s := NewServer(configureRepos(&Config{
		DB: db,
		q:  q,
	}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port
	logger.Debug(fmt.Sprintf("attempting to listen on %q...", addr))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Debug(fmt.Sprintf("error opening listener on %q...: %v", addr, err))
		shutdown.Fatalf("failed to create listener: %v", err)
	}
	logger.Debug("listener aquired")
	shutdown.BeforeExit(func() { l.Close() })
	runServer(s, l)
	logger.Debug("servers stopped")
}

func runServer(s *grpc.Server, l net.Listener) {
	logger.Debug("initializing grpc-web server...")
	grpcWebServer := grpcweb.WrapServer(s)

	logger.Debug("initializing cmux listeners...")
	m := cmux.New(l)
	grpcListener := m.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
	grpcWebListener := m.Match(cmux.Any())

	var wg sync.WaitGroup

	logger.Debug("starting servers...")
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Debug("starting gRPC server...")
		s.Serve(grpcListener)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Debug("starting gRPC-web server...")
		http.Serve(
			grpcWebListener,
			httphelper.ContextInjector(
				"controller-grpc [gRPC-web]",
				httphelper.NewRequestLogger(corsHandler(http.HandlerFunc(grpcWebServer.ServeHttp))),
			),
		)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Debug("starting mux server...")
		m.Serve()
	}()

	wg.Wait()
}

type Config struct {
	DB               *postgres.DB
	q                *que.Client
	appRepo          *data.AppRepo
	artifactRepo     *data.ArtifactRepo
	releaseRepo      *data.ReleaseRepo
	formationRepo    *data.FormationRepo
	deploymentRepo   *data.DeploymentRepo
	eventRepo        *data.EventRepo
	eventListenerMtx sync.Mutex
	eventListener    *data.EventListener
}

func configureRepos(c *Config) *Config {
	c.appRepo = data.NewAppRepo(c.DB, os.Getenv("DEFAULT_ROUTE_DOMAIN"), routerc.New())
	c.artifactRepo = data.NewArtifactRepo(c.DB)
	c.releaseRepo = data.NewReleaseRepo(c.DB, c.artifactRepo, c.q)
	c.formationRepo = data.NewFormationRepo(c.DB, c.appRepo, c.releaseRepo, c.artifactRepo)
	c.eventRepo = data.NewEventRepo(c.DB)
	c.deploymentRepo = data.NewDeploymentRepo(c.DB, c.appRepo, c.releaseRepo, c.formationRepo)
	return c
}

func (c *Config) maybeStartEventListener() (*data.EventListener, error) {
	c.eventListenerMtx.Lock()
	defer c.eventListenerMtx.Unlock()
	if c.eventListener != nil && !c.eventListener.IsClosed() {
		return c.eventListener, nil
	}
	c.eventListener = data.NewEventListener(c.eventRepo)
	return c.eventListener, c.eventListener.Listen()
}

type EventListener struct {
	Events  chan *ct.Event
	Err     error
	errOnce sync.Once
	subs    []*data.EventSubscriber
}

func (e *EventListener) Close() {
	for _, sub := range e.subs {
		sub.Close()
		if err := sub.Err; err != nil {
			e.errOnce.Do(func() { e.Err = err })
		}
	}
}

func (c *Config) subscribeEvents(appIDs []string, objectTypes []ct.EventType, objectIDs []string) (*EventListener, error) {
	dataEventListener, err := c.maybeStartEventListener()
	if err != nil {
		// TODO(jvatic): return proper error code
		return nil, err
	}

	eventListener := &EventListener{
		Events: make(chan *ct.Event),
	}

	objectTypeStrings := make([]string, len(objectTypes))
	for i, t := range objectTypes {
		objectTypeStrings[i] = string(t)
	}

	if len(appIDs) == 0 {
		// an empty string matches all app ids
		appIDs = []string{""}
	}
	subs := make([]*data.EventSubscriber, len(appIDs))
	for i, appID := range appIDs {
		sub, err := dataEventListener.Subscribe(appID, objectTypeStrings, objectIDs)
		if err != nil {
			// TODO(jvatic): return proper error code
			return nil, err
		}
		subs[i] = sub
		go (func() {
			for {
				ctEvent, ok := <-sub.Events
				if !ok {
					break
				}
				eventListener.Events <- ctEvent
			}
		})()
	}
	eventListener.subs = subs
	return eventListener, nil
}

func corsHandler(main http.Handler) http.Handler {
	return (&cors.Options{
		ShouldAllowOrigin: func(origin string, req *http.Request) bool {
			return true
		},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"},
		AllowHeaders:     []string{"Authorization", "Accept", "Content-Type", "If-Match", "If-None-Match", "X-GRPC-Web"},
		ExposeHeaders:    []string{"ETag"},
		AllowCredentials: true,
		MaxAge:           time.Hour,
	}).Handler(main)
}

func NewServer(c *Config) *grpc.Server {
	s := grpc.NewServer(grpc.StatsHandler(&statsHandler{logger: logger.New()}))
	protobuf.RegisterControllerServer(s, &server{Config: c})
	// Register reflection service on gRPC server.
	reflection.Register(s)
	return s
}

type statsHandler struct {
	logger log.Logger
}

func (h *statsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	logger, ok := ctxhelper.LoggerFromContext(ctx)
	if !ok {
		logger = h.logger
	}
	logger.Info("gRPC request started", "rpcMethod", info.FullMethodName)
	return ctx
}

func (h *statsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
}

func (h *statsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *statsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
}

type server struct {
	protobuf.ControllerServer
	*Config
}

func (s *server) listApps(req *protobuf.StreamAppsRequest) ([]*protobuf.App, *data.PageToken, error) {
	pageSize := int(req.GetPageSize())
	pageToken, err := data.ParsePageToken(req.PageToken)
	if err != nil {
		return nil, nil, err
	}

	if pageSize > 0 {
		pageToken.Size = pageSize
	} else {
		pageSize = pageToken.Size
	}

	labelFilters := req.GetLabelFilters()
	ctApps, nextPageToken, err := s.appRepo.ListPage(pageToken)
	if err != nil {
		return nil, nil, err
	}

	if pageSize == 0 {
		pageSize = len(ctApps)
	}

	appIDs := utils.ParseAppIDsFromNameFilters(req.GetNameFilters())
	if len(appIDs) == 0 {
		appIDs = nil
	}

	apps := make([]*protobuf.App, 0, pageSize)
	n := 0

	for _, a := range ctApps {
		// filter apps by ID or Name, TODO(jvatic): move this into the data repo
		if appIDs != nil {
			found := false
			for _, appID := range appIDs {
				if a.ID == appID || a.Name == appID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		if !protobuf.MatchLabelFilters(a.Meta, labelFilters) {
			continue
		}

		apps = append(apps, utils.ConvertApp(a))
		n++

		if n == pageSize {
			break
		}
	}

	// make sure we fill the page if possible
	if n < pageSize && nextPageToken != nil {
		// fetch next page and merge with existing one
		nextApps, npt, err := s.listApps(&protobuf.StreamAppsRequest{
			PageSize:      req.PageSize,
			PageToken:     nextPageToken.String(),
			NameFilters:   req.NameFilters,
			LabelFilters:  req.LabelFilters,
			StreamUpdates: req.StreamUpdates,
			StreamCreates: req.StreamCreates,
		})
		if err != nil {
			return apps, nextPageToken, nil
		}
		nextApps = append(nextApps, apps...)
		return nextApps, npt, nil
	}

	return apps, nextPageToken, nil
}

func (s *server) StreamApps(req *protobuf.StreamAppsRequest, stream protobuf.Controller_StreamAppsServer) error {
	unary := !(req.StreamUpdates || req.StreamCreates)

	var apps []*protobuf.App
	var nextPageToken *data.PageToken
	var appsMtx sync.RWMutex
	refreshApps := func() error {
		appsMtx.Lock()
		defer appsMtx.Unlock()
		var err error
		apps, nextPageToken, err = s.listApps(req)
		return err
	}

	sendResponse := func() {
		appsMtx.RLock()
		stream.Send(&protobuf.StreamAppsResponse{
			Apps:          apps,
			NextPageToken: nextPageToken.String(),
			PageComplete:  true,
		})
		appsMtx.RUnlock()
	}

	var sub *EventListener
	var err error
	if !unary {
		appIDs := utils.ParseAppIDsFromNameFilters(req.GetNameFilters())
		sub, err = s.subscribeEvents(appIDs, []ct.EventType{ct.EventTypeApp, ct.EventTypeAppDeletion, ct.EventTypeAppRelease}, nil)
		if err != nil {
			// TODO(jvatic): return proper error code
			return err
		}
		defer sub.Close()
	}

	if err := refreshApps(); err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	sendResponse()
	if unary {
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			event, ok := <-sub.Events
			if !ok {
				break
			}
			switch event.ObjectType {
			case ct.EventTypeApp:
				var ctApp *ct.App
				if err := json.Unmarshal(event.Data, &ctApp); err != nil {
					// TODO(jvatic): Handle error
					fmt.Printf("StreamApps: Error unmarshalling event.Data -> App: %s\n", err)
					continue
				}
				app := utils.ConvertApp(ctApp)
				shouldSend := false
				if (req.StreamCreates && event.Op == ct.EventOpCreate) || (req.StreamUpdates && event.Op == ct.EventOpUpdate) {
					shouldSend = true
				}
				if !protobuf.MatchLabelFilters(app.Labels, req.GetLabelFilters()) {
					shouldSend = false
				}
				if shouldSend {
					stream.Send(&protobuf.StreamAppsResponse{
						Apps: []*protobuf.App{app},
					})
				}
			case ct.EventTypeAppDeletion:
				if !req.StreamUpdates {
					continue
				}
				// TODO(jvatic)
			case ct.EventTypeAppRelease:
				if !req.StreamUpdates {
					continue
				}
				// TODO(jvatic)
			}
		}
	}()
	wg.Wait()

	if err := sub.Err; err != nil {
		return utils.ConvertError(err, err.Error())
	}

	return nil
}

func (s *server) UpdateApp(ctx context.Context, req *protobuf.UpdateAppRequest) (*protobuf.App, error) {
	app := req.App
	data := map[string]interface{}{
		"meta": app.Labels,
	}

	if app.Strategy != "" {
		data["strategy"] = app.Strategy
	}

	if app.DeployTimeout > 0 {
		data["deploy_timeout"] = app.DeployTimeout
	}

	if mask := req.GetUpdateMask(); mask != nil {
		if paths := mask.GetPaths(); len(paths) > 0 {
			maskedData := make(map[string]interface{}, len(paths))
			for _, path := range paths {
				if path == "labels" {
					path = "meta"
				}
				if v, ok := data[path]; ok {
					maskedData[path] = v
				}
			}
			data = maskedData
		}
	}

	ctApp, err := s.appRepo.Update(utils.ParseIDFromName(app.Name, "apps"), data)
	if err != nil {
		return nil, utils.ConvertError(err, err.Error())
	}
	return utils.ConvertApp(ctApp.(*ct.App)), nil
}

func (s *server) createScale(req *protobuf.CreateScaleRequest) (*protobuf.ScaleRequest, error) {
	appID := utils.ParseIDFromName(req.Parent, "apps")
	releaseID := utils.ParseIDFromName(req.Parent, "releases")
	processes := parseDeploymentProcesses(req.Processes)
	tags := parseDeploymentTags(req.Tags)

	sub, err := s.subscribeEvents([]string{appID}, []ct.EventType{ct.EventTypeScaleRequest}, nil)
	if err != nil {
		// TODO(jvatic): return proper error code
		return nil, err
	}
	defer sub.Close()

	scaleReq := &ct.ScaleRequest{
		AppID:     appID,
		ReleaseID: releaseID,
		State:     ct.ScaleRequestStatePending,
	}
	if processes != nil {
		scaleReq.NewProcesses = &processes
	}
	if tags != nil {
		scaleReq.NewTags = &tags
	}
	if _, err := s.formationRepo.AddScaleRequest(scaleReq, false); err != nil {
		// TODO(jvatic): return proper error code
		return nil, err
	}

	timeout := time.After(ct.DefaultScaleTimeout)
outer:
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				break outer
			}
			switch event.ObjectType {
			case ct.EventTypeScaleRequest:
				var req ct.ScaleRequest
				if err := json.Unmarshal(event.Data, &req); err != nil {
					continue
				}
				if req.ID != scaleReq.ID {
					continue
				}
				switch req.State {
				case ct.ScaleRequestStateCancelled:
					// TODO(jvatic): return proper error code
					return nil, errors.New("scale request cancelled")
				case ct.ScaleRequestStateComplete:
					break outer
				}
			}
		case <-timeout:
			// TODO(jvatic): return proper error code
			return nil, fmt.Errorf("timed out waiting for scale to complete (waited %.f seconds)", ct.DefaultScaleTimeout.Seconds())
		}
	}

	if err := sub.Err; err != nil {
		return nil, utils.ConvertError(err, err.Error())
	}

	return utils.ConvertScaleRequest(scaleReq), nil
}

func (s *server) CreateScale(ctx context.Context, req *protobuf.CreateScaleRequest) (*protobuf.ScaleRequest, error) {
	return s.createScale(req)
}

func (s *server) StreamScales(req *protobuf.StreamScalesRequest, stream protobuf.Controller_StreamScalesServer) error {
	unary := !(req.StreamUpdates || req.StreamCreates)

	pageSize := int(req.PageSize)
	pageToken, err := data.ParsePageToken(req.PageToken)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}

	if pageSize > 0 {
		pageToken.Size = pageSize
	} else {
		pageSize = pageToken.Size
	}

	appIDs := utils.ParseIDsFromNameFilters(req.NameFilters, "apps")
	releaseIDs := utils.ParseIDsFromNameFilters(req.NameFilters, "releases")
	scaleIDs := utils.ParseIDsFromNameFilters(req.NameFilters, "scales")

	streamAppIDs := appIDs
	streamScaleIDs := scaleIDs
	if len(releaseIDs) > 0 {
		// we can't filter releaseIDs in the subscription, so don't filter anything
		streamAppIDs = nil
		streamScaleIDs = nil
	}
	sub, err := s.subscribeEvents(streamAppIDs, []ct.EventType{ct.EventTypeScaleRequest}, streamScaleIDs)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	defer sub.Close()

	// get all events up until now
	list, nextPageToken, err := s.formationRepo.ListScaleRequests(data.ListScaleRequestOptions{
		PageToken:  *pageToken,
		AppIDs:     appIDs,
		ReleaseIDs: releaseIDs,
		ScaleIDs:   scaleIDs,
	})
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	scaleRequests := make([]*protobuf.ScaleRequest, 0, len(list))
	for _, ctScale := range list {
		scaleRequests = append(scaleRequests, utils.ConvertScaleRequest(ctScale))
	}
	stream.Send(&protobuf.StreamScalesResponse{
		ScaleRequests: scaleRequests,
		PageComplete:  true,
		NextPageToken: nextPageToken.String(),
	})

	if unary {
		return nil
	}

	releaseIDsMap := make(map[string]struct{}, len(releaseIDs))
	for _, releaseID := range releaseIDs {
		releaseIDsMap[releaseID] = struct{}{}
	}

	appIDsMap := make(map[string]struct{}, len(appIDs))
	for _, appID := range appIDs {
		appIDsMap[appID] = struct{}{}
	}

	scaleIDsMap := make(map[string]struct{}, len(scaleIDs))
	for _, scaleID := range scaleIDs {
		scaleIDsMap[scaleID] = struct{}{}
	}

	unmarshalScaleRequest := func(event *ct.Event) (*protobuf.ScaleRequest, error) {
		var ctReq *ct.ScaleRequest
		if err := json.Unmarshal(event.Data, &ctReq); err != nil {
			// TODO(jvatic): return proper error code
			return nil, err
		}
		return utils.ConvertScaleRequest(ctReq), nil
	}

	// stream new events as they are created
	var currID int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			event, ok := <-sub.Events
			if !ok {
				break
			}

			// avoid overlap between list and stream
			if event.ID <= currID {
				continue
			}
			currID = event.ID

			if !((req.StreamCreates && event.Op == ct.EventOpCreate) || (req.StreamUpdates && event.Op == ct.EventOpUpdate)) {
				// EventOp doesn't match the stream type
				continue
			}

			scale, err := unmarshalScaleRequest(event)
			if err != nil {
				// TODO(jvatic): Handle error
				fmt.Printf("ScaleRequestsStream(%v): Error parsing data: %s\n", req.NameFilters, err)
				continue
			}

			releaseIDMatches := false
			if len(releaseIDsMap) > 0 {
				if _, ok := releaseIDsMap[utils.ParseIDFromName(scale.Name, "releases")]; ok {
					releaseIDMatches = true
				}
			}

			appIDMatches := false
			if len(appIDsMap) > 0 {
				if _, ok := appIDsMap[utils.ParseIDFromName(scale.Name, "apps")]; ok {
					appIDMatches = true
				}
			}

			scaleIDMatches := false
			if len(scaleIDsMap) > 0 {
				if _, ok := scaleIDsMap[utils.ParseIDFromName(scale.Name, "scales")]; ok {
					scaleIDMatches = true
				}
			}

			if !(releaseIDMatches || appIDMatches || scaleIDMatches) {
				if len(releaseIDsMap) > 0 || len(appIDsMap) > 0 || len(scaleIDsMap) > 0 {
					continue
				}
			}

			stream.Send(&protobuf.StreamScalesResponse{
				ScaleRequests: []*protobuf.ScaleRequest{scale},
			})
		}
	}()
	wg.Wait()

	// TODO(jvatic): return proper error code
	return sub.Err
}

func (s *server) StreamReleases(req *protobuf.StreamReleasesRequest, stream protobuf.Controller_StreamReleasesServer) error {
	unary := !(req.StreamUpdates || req.StreamCreates)
	pageSize := int(req.PageSize)
	pageToken, err := data.ParsePageToken(req.PageToken)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}

	if pageSize > 0 {
		pageToken.Size = pageSize
	} else {
		pageSize = pageToken.Size
	}

	eventAppIDs := utils.ParseAppIDsFromNameFilters(req.NameFilters)
	appIDs := make(map[string]struct{}, len(eventAppIDs))
	for _, appID := range eventAppIDs {
		appIDs[appID] = struct{}{}
	}

	releaseIDs := make(map[string]struct{}, len(req.NameFilters))
	for _, releaseID := range utils.ParseReleaseIDsFromNameFilters(req.NameFilters) {
		releaseIDs[releaseID] = struct{}{}
	}

	if len(releaseIDs) > 0 {
		// the events list and stream can't currently handle both types of ID
		// filters. TODO(jvatic): make events repo filter by multiple object IDs
		eventAppIDs = nil
	}

	var nextPageToken *data.PageToken
	var releases []*protobuf.Release
	var releasesMtx sync.RWMutex
	sendResponse := func() {
		releasesMtx.RLock()
		defer releasesMtx.RUnlock()
		stream.Send(&protobuf.StreamReleasesResponse{
			Releases:      releases,
			NextPageToken: nextPageToken.String(),
			PageComplete:  true,
		})
	}

	unmarshalRelease := func(event *ct.Event) (*protobuf.Release, error) {
		var ctRelease *ct.Release
		if err := json.Unmarshal(event.Data, &ctRelease); err != nil {
			// TODO(jvatic): return proper error code
			return nil, err
		}
		return utils.ConvertRelease(ctRelease), nil
	}

	maybeAcceptRelease := func(event *ct.Event) (release *protobuf.Release, accepted bool, err error) {
		// apply name filters
		if len(releaseIDs) > 0 {
			if _, ok := releaseIDs[event.ObjectID]; !ok {
				if len(appIDs) > 0 {
					if _, ok := appIDs[event.AppID]; !ok {
						return
					}
				} else {
					return
				}
			}
		}

		var r *protobuf.Release
		r, err = unmarshalRelease(event)
		if err != nil {
			return
		}

		if !protobuf.MatchLabelFilters(r.Labels, req.GetLabelFilters()) {
			return
		}

		accepted = true
		release = r
		return
	}

	maybePrependRelease := func(event *ct.Event) error {
		r, accepted, err := maybeAcceptRelease(event)
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}

		releasesMtx.Lock()
		_releases := make([]*protobuf.Release, 0, len(releases)+1)
		_releases = append(_releases, r)
		for _, sr := range releases {
			if sr.GetName() == r.GetName() {
				continue
			}
			_releases = append(_releases, sr)
		}
		releases = _releases
		releasesMtx.Unlock()
		return nil
	}

	sub, err := s.subscribeEvents(eventAppIDs, []ct.EventType{ct.EventTypeRelease}, nil)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	defer sub.Close()

	// get all events up until now
	var currID int64
	count := pageSize
	if count > 0 {
		count = count + 1 // request 1 more than what we need to see if there's a next page
	}
	list, err := s.eventRepo.LegacyListEvents(eventAppIDs, []string{string(ct.EventTypeRelease)}, nil, pageToken.BeforeIDInt64(), nil, count)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	if pageToken.BeforeIDInt64() != nil {
		currID = *pageToken.BeforeIDInt64()
	}
	if (pageSize == 0 || len(list) == pageSize+1) && len(list) > 0 {
		list = list[0 : len(list)-1] // get rid of extra event
		event := list[0]
		beforeID := fmt.Sprintf("%d", event.ID)
		nextPageToken = &data.PageToken{BeforeID: &beforeID, Size: pageSize}
	}
	// list is in DESC order, so iterate in reverse
	for i := len(list) - 1; i >= 0; i-- {
		event := list[i]
		currID = event.ID

		if err := maybePrependRelease(event); err != nil {
			// TODO(jvatic): Handle error
			fmt.Printf("ReleasesStream(%v): Error parsing data: %s\n", req.NameFilters, err)
			continue
		}
	}
	sendResponse()

	if unary {
		return nil
	}

	// stream new events as they are created
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			event, ok := <-sub.Events
			if !ok {
				break
			}

			// avoid overlap between list and stream
			if event.ID <= currID {
				continue
			}
			currID = event.ID

			release, ok, err := maybeAcceptRelease(event)
			if err != nil {
				// TODO(jvatic): Handle error
				fmt.Printf("ReleasesStream(%v): Error parsing data: %s\n", req.NameFilters, err)
				continue
			}

			if ok {
				stream.Send(&protobuf.StreamReleasesResponse{
					Releases: []*protobuf.Release{release},
				})
			}
		}
	}()
	wg.Wait()

	// TODO(jvatic): return proper error code
	return sub.Err
}

func (s *server) CreateRelease(ctx context.Context, req *protobuf.CreateReleaseRequest) (*protobuf.Release, error) {
	ctRelease := utils.BackConvertRelease(req.Release)
	ctRelease.AppID = utils.ParseIDFromName(req.Parent, "apps")
	if err := s.releaseRepo.Add(ctRelease); err != nil {
		// TODO(jvatic): return proper error code
		return nil, err
	}
	return utils.ConvertRelease(ctRelease), nil
}

func (s *server) listDeployments(req *protobuf.StreamDeploymentsRequest) ([]*protobuf.ExpandedDeployment, *data.PageToken, error) {
	pageToken, err := data.ParsePageToken(req.PageToken)
	if err != nil {
		return nil, nil, err
	}
	if req.PageSize > 0 {
		pageToken.Size = int(req.PageSize)
	}
	ctExpandedDeployments, nextPageToken, err := s.deploymentRepo.ListPage(data.ListDeploymentOptions{
		PageToken:     *pageToken,
		AppIDs:        utils.ParseIDsFromNameFilters(req.NameFilters, "apps"),
		DeploymentIDs: utils.ParseIDsFromNameFilters(req.NameFilters, "deployments"),
	})
	if err != nil {
		return nil, nil, err
	}

	deployments := make([]*protobuf.ExpandedDeployment, 0, len(ctExpandedDeployments))
	for _, d := range ctExpandedDeployments {
		deployments = append(deployments, utils.ConvertExpandedDeployment(d))
	}

	var filtered []*protobuf.ExpandedDeployment
	typeMatcher := protobuf.NewReleaseTypeMatcher(req.TypeFilters)
	if len(req.TypeFilters) == 0 {
		filtered = deployments
	} else {
		filtered = make([]*protobuf.ExpandedDeployment, 0, len(deployments))
		for _, ed := range deployments {
			// filter by type of deployment
			if !typeMatcher.Match(ed.Type) {
				continue
			}
			filtered = append(filtered, ed)
		}
	}

	return filtered, nextPageToken, nil
}

func (s *server) StreamDeployments(req *protobuf.StreamDeploymentsRequest, stream protobuf.Controller_StreamDeploymentsServer) error {
	unary := !(req.StreamUpdates || req.StreamCreates)

	appIDs := utils.ParseIDsFromNameFilters(req.NameFilters, "apps")
	deploymentIDs := utils.ParseIDsFromNameFilters(req.NameFilters, "deployments")

	var deploymentsMtx sync.RWMutex
	var deployments []*protobuf.ExpandedDeployment
	var nextPageToken *data.PageToken
	refreshDeployments := func() error {
		deploymentsMtx.Lock()
		defer deploymentsMtx.Unlock()
		var err error
		deployments, nextPageToken, err = s.listDeployments(req)
		return err
	}

	sendResponse := func() {
		deploymentsMtx.RLock()
		stream.Send(&protobuf.StreamDeploymentsResponse{
			Deployments:   deployments,
			PageComplete:  true,
			NextPageToken: nextPageToken.String(),
		})
		deploymentsMtx.RUnlock()
	}

	if err := refreshDeployments(); err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	sendResponse()

	if unary {
		return nil
	}

	var wg sync.WaitGroup

	sub, err := s.subscribeEvents(appIDs, []ct.EventType{ct.EventTypeDeployment}, deploymentIDs)
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	defer sub.Close()

	wg.Add(1)
	go func() {
		defer wg.Done()
		typeMatcher := protobuf.NewReleaseTypeMatcher(req.TypeFilters)
		for {
			ctEvent, ok := <-sub.Events
			if !ok {
				break
			}

			if !((req.StreamCreates && ctEvent.Op == ct.EventOpCreate) || (req.StreamUpdates && ctEvent.Op == ct.EventOpUpdate)) {
				// EventOp doesn't match the stream type
				continue
			}

			var deploymentEvent *ct.DeploymentEvent
			if err := json.Unmarshal(ctEvent.Data, &deploymentEvent); err != nil {
				// TODO(jvatic): handle error
				fmt.Printf("StreamDeployments Error unmarshalling event: %v\n", err)
				continue
			}
			ctd, err := s.deploymentRepo.GetExpanded(ctEvent.ObjectID)
			if err != nil {
				// TODO(jvatic): handle error
				fmt.Printf("StreamDeployments Error fetching deployment: %v\n", err)
				continue
			}
			ctd.Status = deploymentEvent.Status
			d := utils.ConvertExpandedDeployment(ctd)
			if !typeMatcher.Match(d.Type) {
				continue
			}
			stream.Send(&protobuf.StreamDeploymentsResponse{
				Deployments: []*protobuf.ExpandedDeployment{d},
			})
		}
	}()
	wg.Wait()

	// TODO(jvatic): return proper error code
	return sub.Err
}

func parseDeploymentTags(from map[string]*protobuf.DeploymentProcessTags) map[string]map[string]string {
	to := make(map[string]map[string]string, len(from))
	for k, v := range from {
		to[k] = v.Tags
	}
	return to
}

func parseDeploymentProcesses(from map[string]int32) map[string]int {
	to := make(map[string]int, len(from))
	for k, v := range from {
		to[k] = int(v)
	}
	return to
}

func (s *server) CreateDeployment(req *protobuf.CreateDeploymentRequest, ds protobuf.Controller_CreateDeploymentServer) error {
	appID := utils.ParseIDFromName(req.Parent, "apps")
	d, err := s.deploymentRepo.Add(appID, utils.ParseIDFromName(req.Release, "releases"))
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}

	// Wait for deployment to complete and perform scale

	sub, err := s.subscribeEvents([]string{appID}, []ct.EventType{ct.EventTypeDeployment}, []string{d.ID})
	if err != nil {
		// TODO(jvatic): return proper error code
		return err
	}
	defer sub.Close()

	for {
		ctEvent, ok := <-sub.Events
		if !ok {
			break
		}
		if ctEvent.ObjectType != "deployment" {
			continue
		}
		var de *ct.DeploymentEvent
		if err := json.Unmarshal(ctEvent.Data, &de); err != nil {
			fmt.Printf("Failed to unmarshal deployment event(%s): %s\n", ctEvent.ObjectID, err)
			continue
		}

		d, err := s.deploymentRepo.Get(ctEvent.ObjectID)
		if err != nil {
			fmt.Printf("Failed to get deployment(%s): %s\n", ctEvent.ObjectID, err)
			continue
		}

		// Scale release to requested processes/tags once deployment is complete
		if d.Status == "complete" {
			if sr := req.ScaleRequest; sr != nil {
				s.createScale(&protobuf.CreateScaleRequest{
					Parent:    fmt.Sprintf("apps/%s/releases/%s", d.AppID, d.NewReleaseID),
					Processes: sr.Processes,
					Tags:      sr.Tags,
				})
			}
		}

		ds.Send(&protobuf.DeploymentEvent{
			Deployment: utils.ConvertDeployment(d),
			JobType:    de.JobType,
			JobState:   utils.ConvertDeploymentEventJobState(de.JobState),
			Error:      de.Error,
			CreateTime: utils.TimestampProto(ctEvent.CreatedAt),
		})

		if d.Status == "failed" {
			// TODO(jvatic): return proper error code
			return fmt.Errorf(de.Error)
		}
		if d.Status == "complete" {
			break
		}
	}

	// TODO(jvatic): return proper error code
	return sub.Err
}
