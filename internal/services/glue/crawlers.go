package glue

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindCrawler  = "crawler"
	crawlerIndex = "aws/glue/index/crawler/"
)

type crawlerRecord struct {
	Crawler        api.Crawler
	Metrics        api.CrawlerMetrics
	Metadata       request.Metadata
	Tags           map[string]string
	SeenFolders    map[string]bool
	Owner, CrawlID string
	Runtimes       []float64
	Next           float64
}

type crawlRecord struct {
	History                                 api.CrawlerHistory
	Crawler, Workflow, WorkflowRun, Trigger string
}

func crawlerIndexKey(scope request.Scope, n string) string {
	return crawlerIndex + scope.AccountID + "/" + scope.Region + "/" + url.PathEscape(n)
}

func loadCrawler(tx storage.ReadTx, scope request.Scope, n string) (*crawlerRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[crawlerRecord](tx, orchestrationKey(scope, kindCrawler, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Crawler %s not found", n)
	}

	return r, nil
}

func (s *Service) validateCrawler(ctx context.Context, c *api.Crawler) error {
	if err := orchestrationName(c.Name); err != nil {
		return err
	}

	if err := s.validateRole(ctx, c.Role); err != nil {
		return err
	}

	if c.DatabaseName == nil {
		return invalid("DatabaseName is required for S3 targets")
	}

	db, err := name("database", c.DatabaseName)
	if err != nil {
		return err
	}

	c.DatabaseName = new(db)
	if c.Description != nil && len(*c.Description) > 2048 {
		return invalid("Description exceeds 2048 bytes")
	}

	if c.TablePrefix != nil && len(*c.TablePrefix) > 128 {
		return invalid("TablePrefix exceeds 128 bytes")
	}

	if c.Targets == nil || len(c.Targets.S3Targets) == 0 {
		return invalid("At least one S3 target is required")
	}

	for _, t := range c.Targets.S3Targets {
		u, err := url.Parse(common.Value(t.Path))
		if err != nil || u.Scheme != "s3" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return invalid("S3 target Path must be s3://bucket/prefix")
		}

		// Terraform sends these as empty strings when unset; connections and S3 event crawls are not emulated.
		if common.Value(t.ConnectionName) != "" || common.Value(t.EventQueueArn) != "" ||
			common.Value(t.DlqEventQueueArn) != "" {
			return invalid("S3 target connections and event queues are not supported")
		}

		if t.SampleSize != nil && (*t.SampleSize < 1 || *t.SampleSize > 249) {
			return invalid("SampleSize must be between 1 and 249")
		}

		if _, err := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: new(u.Host)}); err != nil {
			return invalid("S3 target bucket does not exist")
		}

		for _, g := range t.Exclusions {
			if _, err := crawlGlob(g); err != nil {
				return invalid("Invalid exclusion: %s", err)
			}
		}
	}

	if c.Schedule != nil {
		if err := validateSchedule(common.Value(c.Schedule.ScheduleExpression)); err != nil {
			return err
		}
	}

	if c.SchemaChangePolicy == nil {
		c.SchemaChangePolicy = &api.SchemaChangePolicy{}
	}

	p := c.SchemaChangePolicy
	if p.UpdateBehavior == nil {
		p.UpdateBehavior = new("UPDATE_IN_DATABASE")
	}

	if p.DeleteBehavior == nil {
		p.DeleteBehavior = new(deprecateInDatabase)
	}

	if !slices.Contains([]string{behaviorLog, "UPDATE_IN_DATABASE"}, *p.UpdateBehavior) ||
		!slices.Contains([]string{behaviorLog, deleteFromDatabase, deprecateInDatabase}, *p.DeleteBehavior) {
		return invalid("Invalid SchemaChangePolicy")
	}

	if c.RecrawlPolicy == nil {
		c.RecrawlPolicy = &api.RecrawlPolicy{RecrawlBehavior: new("CRAWL_EVERYTHING")}
	}

	if !slices.Contains(
		[]string{"CRAWL_EVERYTHING", recrawlNewFolders},
		common.Value(c.RecrawlPolicy.RecrawlBehavior),
	) {
		return invalid("Unsupported RecrawlBehavior")
	}

	if *c.RecrawlPolicy.RecrawlBehavior == recrawlNewFolders &&
		(*p.UpdateBehavior != behaviorLog || *p.DeleteBehavior != behaviorLog) {
		return invalid("CRAWL_NEW_FOLDERS_ONLY requires LOG update and delete policies")
	}

	if c.Configuration != nil {
		var conf map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*c.Configuration), &conf); err != nil || conf == nil {
			return invalid("Configuration must be a JSON object")
		}

		for k, v := range conf {
			if k != "Version" {
				return invalid("Unsupported Configuration key %s", k)
			}

			var version float64
			if err := json.Unmarshal(v, &version); err != nil || version != 1 {
				return invalid("Configuration Version must be 1.0")
			}
		}
	}

	return s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		for _, n := range c.Classifiers {
			if _, err := loadClassifier(tx, scope, n); err != nil {
				return err
			}
		}

		return nil
	})
}

func (s *Service) CreateCrawler(ctx context.Context, in *api.CreateCrawlerRequest) (*api.CreateCrawlerResponse, error) {
	c := api.Crawler{
		Name:               in.Name,
		Role:               in.Role,
		DatabaseName:       in.DatabaseName,
		Description:        in.Description,
		Targets:            in.Targets,
		Classifiers:        in.Classifiers,
		TablePrefix:        in.TablePrefix,
		SchemaChangePolicy: in.SchemaChangePolicy,
		RecrawlPolicy:      in.RecrawlPolicy,
		Configuration:      in.Configuration,
	}

	if in.Schedule != nil && *in.Schedule != "" {
		c.Schedule = &api.Schedule{ScheduleExpression: in.Schedule, State: new(triggerScheduled)}
	}

	if err := s.validateCrawler(ctx, &c); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	c.State = new(stateReady)
	c.Version = new(int64(1))
	c.CreationTime = new(common.Unix(s.now()))
	c.LastUpdated = c.CreationTime
	metadata, _ := request.FromContext(ctx)
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		key := orchestrationKey(scope, kindCrawler, *c.Name)
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Crawler already exists")
		}

		c.CatalogId = new(scope.AccountID)
		r := &crawlerRecord{Crawler: c, Tags: in.Tags, Metadata: metadata, SeenFolders: map[string]bool{}}
		s.nextCrawlerSchedule(r)
		if err := storage.Write(tx, key, r); err != nil {
			return err
		}

		return storage.Write(tx, crawlerIndexKey(scope, *c.Name), triggerReference{Scope: scope, Name: *c.Name})
	})

	return &api.CreateCrawlerResponse{}, err
}

func (s *Service) GetCrawler(ctx context.Context, in *api.GetCrawlerRequest) (*api.GetCrawlerResponse, error) {
	out := &api.GetCrawlerResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadCrawler(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		out.Crawler = &r.Crawler
		return nil
	})

	return out, err
}

func (s *Service) crawlerPage(
	ctx context.Context,
	tags map[string]string,
	token *string,
	limit *int64,
) ([]*crawlerRecord, *string, error) {
	var rows []*crawlerRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := storage.Records[crawlerRecord](tx, scope.Prefix(service, kindCrawler))
		if err != nil {
			return err
		}

		for _, r := range all {
			if tagsMatch(r.Tags, tags) {
				rows = append(rows, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return orchestrationPage(rows, func(r *crawlerRecord) string { return *r.Crawler.Name }, token, limit)
}

func (s *Service) GetCrawlers(ctx context.Context, in *api.GetCrawlersRequest) (*api.GetCrawlersResponse, error) {
	rows, next, err := s.crawlerPage(ctx, nil, in.NextToken, in.MaxResults)
	out := &api.GetCrawlersResponse{Crawlers: []api.Crawler{}, NextToken: next}
	for _, r := range rows {
		out.Crawlers = append(out.Crawlers, r.Crawler)
	}

	return out, err
}

func (s *Service) ListCrawlers(ctx context.Context, in *api.ListCrawlersRequest) (*api.ListCrawlersResponse, error) {
	rows, next, err := s.crawlerPage(ctx, in.Tags, in.NextToken, in.MaxResults)
	out := &api.ListCrawlersResponse{CrawlerNames: []string{}, NextToken: next}
	for _, r := range rows {
		out.CrawlerNames = append(out.CrawlerNames, *r.Crawler.Name)
	}

	return out, err
}

func (s *Service) BatchGetCrawlers(
	ctx context.Context,
	in *api.BatchGetCrawlersRequest,
) (*api.BatchGetCrawlersResponse, error) {
	if len(in.CrawlerNames) == 0 {
		return nil, invalid("CrawlerNames must not be empty")
	}

	out := &api.BatchGetCrawlersResponse{Crawlers: []api.Crawler{}, CrawlersNotFound: []string{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		for _, n := range in.CrawlerNames {
			if err := orchestrationName(new(n)); err != nil {
				return err
			}

			r, ok, err := storage.Read[crawlerRecord](tx, orchestrationKey(scope, kindCrawler, n))
			if err != nil {
				return err
			}

			if ok {
				out.Crawlers = append(out.Crawlers, r.Crawler)
			} else {
				out.CrawlersNotFound = append(out.CrawlersNotFound, n)
			}
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateCrawler(ctx context.Context, in *api.UpdateCrawlerRequest) (*api.UpdateCrawlerResponse, error) {
	current, err := s.GetCrawler(ctx, &api.GetCrawlerRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}

	c := current.Crawler
	c.Role = common.OrElse(in.Role, c.Role)
	c.DatabaseName = common.OrElse(in.DatabaseName, c.DatabaseName)
	c.Description = common.OrElse(in.Description, c.Description)
	c.Targets = common.OrElse(in.Targets, c.Targets)
	c.TablePrefix = common.OrElse(in.TablePrefix, c.TablePrefix)
	c.SchemaChangePolicy = common.OrElse(in.SchemaChangePolicy, c.SchemaChangePolicy)
	c.RecrawlPolicy = common.OrElse(in.RecrawlPolicy, c.RecrawlPolicy)
	c.Configuration = common.OrElse(in.Configuration, c.Configuration)
	if in.Classifiers != nil {
		c.Classifiers = in.Classifiers
	}

	if in.Schedule != nil {
		c.Schedule = nil
		if *in.Schedule != "" {
			c.Schedule = &api.Schedule{ScheduleExpression: in.Schedule, State: new(triggerScheduled)}
		}
	}

	if validateErr := s.validateCrawler(ctx, c); validateErr != nil {
		return nil, validateErr
	}

	err = s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, loadErr := loadCrawler(tx, scope, common.Value(in.Name))
		if loadErr != nil {
			return loadErr
		}

		if common.Value(r.Crawler.State) != stateReady {
			return awserror.New("CrawlerRunningException", "Crawler is running")
		}

		if common.Value(r.Crawler.Version) != common.Value(c.Version) {
			return awserror.New("VersionMismatchException", "Crawler changed concurrently")
		}

		c.Version = new(common.Value(c.Version) + 1)
		c.LastUpdated = new(common.Unix(s.now()))
		r.Crawler = *c
		s.nextCrawlerSchedule(r)
		return storage.Write(tx, orchestrationKey(scope, kindCrawler, *in.Name), r)
	})

	return &api.UpdateCrawlerResponse{}, err
}

func (s *Service) DeleteCrawler(ctx context.Context, in *api.DeleteCrawlerRequest) (*api.DeleteCrawlerResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadCrawler(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		if common.Value(r.Crawler.State) != stateReady {
			return awserror.New("CrawlerRunningException", "Crawler is running")
		}

		tx.Delete(orchestrationKey(scope, kindCrawler, *in.Name))
		tx.Delete(crawlerIndexKey(scope, *in.Name))
		for _, key := range tx.Keys(crawlPrefix(scope, *in.Name)) {
			tx.Delete(key)
		}

		return nil
	})

	return &api.DeleteCrawlerResponse{}, err
}

func crawlPrefix(scope request.Scope, n string) string {
	return scope.Key(service, "crawl", url.PathEscape(n)+"/")
}

func (s *Service) startCrawler(tx storage.WriteTx, scope request.Scope, n, trigger, workflow, run string) error {
	r, err := loadCrawler(tx, scope, n)
	if err != nil {
		return err
	}

	if common.Value(r.Crawler.State) != stateReady {
		return awserror.New("CrawlerRunningException", "Crawler is running")
	}

	r.Crawler.State = new(stateRunning)
	r.Owner = ""
	r.CrawlID = common.RandomID(32)
	h := api.CrawlerHistory{
		CrawlId:       new(r.CrawlID),
		State:         new(stateRunning),
		StartTime:     new(common.Unix(s.now())),
		LogGroup:      new("/aws-glue/crawlers"),
		LogStream:     new(n),
		MessagePrefix: new(r.CrawlID),
	}

	if err := storage.Write(
		tx,
		crawlPrefix(scope, n)+r.CrawlID,
		crawlRecord{History: h, Crawler: n, Trigger: trigger, Workflow: workflow, WorkflowRun: run},
	); err != nil {
		return err
	}

	return storage.Write(tx, orchestrationKey(scope, kindCrawler, n), r)
}

func (s *Service) StartCrawler(ctx context.Context, in *api.StartCrawlerRequest) (*api.StartCrawlerResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		return s.startCrawler(tx, scope, common.Value(in.Name), "", "", "")
	})

	return &api.StartCrawlerResponse{}, err
}

func (s *Service) StopCrawler(ctx context.Context, in *api.StopCrawlerRequest) (*api.StopCrawlerResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadCrawler(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		switch common.Value(r.Crawler.State) {
		case stateReady:
			return awserror.New("CrawlerNotRunningException", "Crawler is not running")
		case stateStopping:
			return awserror.New("CrawlerStoppingException", "Crawler is stopping")
		}

		r.Crawler.State = new(stateStopping)
		return storage.Write(tx, orchestrationKey(scope, kindCrawler, *in.Name), r)
	})

	return &api.StopCrawlerResponse{}, err
}

func (s *Service) nextCrawlerSchedule(r *crawlerRecord) {
	r.Next = 0
	if r.Crawler.Schedule == nil || common.Value(r.Crawler.Schedule.State) != triggerScheduled {
		return
	}

	plan, err := schedule.Parse(common.Value(r.Crawler.Schedule.ScheduleExpression))
	if err != nil {
		return
	}

	if next, ok := plan.First(s.now()); ok {
		r.Next = common.Unix(next)
	}
}

func (s *Service) crawlerSchedule(ctx context.Context, n, expression *string, state string) error {
	if common.Value(expression) != "" {
		if err := validateSchedule(*expression); err != nil {
			return err
		}
	}

	return s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadCrawler(tx, scope, common.Value(n))
		if err != nil {
			return err
		}

		if expression != nil {
			if *expression == "" {
				r.Crawler.Schedule = nil
			} else {
				r.Crawler.Schedule = &api.Schedule{ScheduleExpression: expression, State: new(triggerScheduled)}
			}
		} else {
			if r.Crawler.Schedule == nil {
				return invalid("Crawler has no schedule")
			}

			r.Crawler.Schedule.State = new(state)
		}

		s.nextCrawlerSchedule(r)
		return storage.Write(tx, orchestrationKey(scope, kindCrawler, *n), r)
	})
}

func (s *Service) UpdateCrawlerSchedule(
	ctx context.Context,
	in *api.UpdateCrawlerScheduleRequest,
) (*api.UpdateCrawlerScheduleResponse, error) {
	return &api.UpdateCrawlerScheduleResponse{}, s.crawlerSchedule(ctx, in.CrawlerName, in.Schedule, "")
}

func (s *Service) StartCrawlerSchedule(
	ctx context.Context,
	in *api.StartCrawlerScheduleRequest,
) (*api.StartCrawlerScheduleResponse, error) {
	return &api.StartCrawlerScheduleResponse{}, s.crawlerSchedule(ctx, in.CrawlerName, nil, triggerScheduled)
}

func (s *Service) StopCrawlerSchedule(
	ctx context.Context,
	in *api.StopCrawlerScheduleRequest,
) (*api.StopCrawlerScheduleResponse, error) {
	return &api.StopCrawlerScheduleResponse{}, s.crawlerSchedule(ctx, in.CrawlerName, nil, "NOT_SCHEDULED")
}

func (s *Service) GetCrawlerMetrics(
	ctx context.Context,
	in *api.GetCrawlerMetricsRequest,
) (*api.GetCrawlerMetricsResponse, error) {
	rows, _, err := s.crawlerPage(ctx, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	var metrics []api.CrawlerMetrics
	for _, r := range rows {
		if len(in.CrawlerNameList) > 0 && !slices.Contains(in.CrawlerNameList, *r.Crawler.Name) {
			continue
		}

		m := r.Metrics
		m.CrawlerName = r.Crawler.Name
		m.StillEstimating = new(common.Value(r.Crawler.State) != stateReady)
		m.TimeLeftSeconds = new(float64(0))
		metrics = append(metrics, m)
	}

	selected, next, err := orchestrationPage(
		metrics,
		func(m api.CrawlerMetrics) string { return *m.CrawlerName },
		in.NextToken,
		in.MaxResults,
	)
	return &api.GetCrawlerMetricsResponse{CrawlerMetricsList: selected, NextToken: next}, err
}

func (s *Service) ListCrawls(ctx context.Context, in *api.ListCrawlsRequest) (*api.ListCrawlsResponse, error) {
	for _, f := range in.Filters {
		if common.Value(f.FieldName) != "STATE" || common.Value(f.FilterOperator) != "EQ" {
			return nil, invalid("Only STATE EQ crawl filters are supported")
		}
	}

	out := &api.ListCrawlsResponse{Crawls: []api.CrawlerHistory{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadCrawler(tx, scope, common.Value(in.CrawlerName)); err != nil {
			return err
		}

		rows, err := storage.Records[crawlRecord](tx, crawlPrefix(scope, *in.CrawlerName))
		if err != nil {
			return err
		}

		var all []api.CrawlerHistory
		for _, r := range rows {
			match := true
			for _, f := range in.Filters {
				if common.Value(r.History.State) != common.Value(f.FieldValue) {
					match = false
				}
			}

			if match {
				all = append(all, r.History)
			}
		}

		out.Crawls, out.NextToken, err = page(
			all,
			func(h api.CrawlerHistory) string { return *h.CrawlId },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})

	return out, err
}
