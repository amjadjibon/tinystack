package glue

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tabular"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// crawlGlob implements Glue's path globs, including ** across directory boundaries.
func crawlGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(pattern[i+1:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unclosed character class")
			}

			// Glue globs negate a class with a leading !, regular expressions with ^.
			class := pattern[i+1 : i+j+1]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}

			b.WriteString("[" + class + "]")
			i += j + 1
		case '{', '}':
			return nil, fmt.Errorf("brace alternation exclusions are not supported")
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}

	b.WriteString("$")
	return regexp.Compile(b.String())
}

func (s *Service) tickCrawlers(ctx context.Context) {
	var refs []*triggerReference
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		refs, err = storage.Records[triggerReference](tx, crawlerIndex)
		return err
	}); err != nil {
		return
	}

	for _, ref := range refs {
		var launch *crawlerRecord
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, err := loadCrawler(tx, ref.Scope, ref.Name)
			if err != nil {
				return err
			}

			state := common.Value(r.Crawler.State)
			if state == stateReady && r.Next > 0 && common.Unix(s.now()) >= r.Next {
				if startErr := s.startCrawler(tx, ref.Scope, ref.Name, "", "", ""); startErr != nil {
					return startErr
				}

				r, err = loadCrawler(tx, ref.Scope, ref.Name)
				if err != nil {
					return err
				}

				s.nextCrawlerSchedule(r)
				state = "RUNNING"
			}

			if state == stateReady {
				return nil
			}

			if r.Owner != "" && r.Owner != s.session {
				return s.finishCrawl(tx, ref.Scope, r, stateFailed, "Crawl interrupted by service restart", nil)
			}

			if r.Owner != "" {
				return nil
			}

			if state == stateStopping {
				return s.finishCrawl(tx, ref.Scope, r, stateCancelled, "", nil)
			}

			r.Owner = s.session
			launch = common.Clone(r)
			return storage.Write(tx, orchestrationKey(ref.Scope, kindCrawler, ref.Name), r)
		})

		if err == nil && launch != nil {
			s.workers.Go(func(parent context.Context) { s.executeCrawl(parent, ref.Scope, launch) })
		}
	}
}

func (s *Service) finishCrawl(
	tx storage.WriteTx,
	scope request.Scope,
	r *crawlerRecord,
	status, message string,
	metrics *api.CrawlerMetrics,
) error {
	h, ok, err := storage.Read[crawlRecord](tx, crawlPrefix(scope, *r.Crawler.Name)+r.CrawlID)
	if err != nil {
		return err
	}

	if !ok {
		return notFound("Crawl history missing")
	}

	if common.Value(r.Crawler.State) == stateStopping {
		status = stateCancelled
		message = ""
	}

	now := common.Unix(s.now())
	h.History.State = new(status)
	h.History.EndTime = new(now)
	if message != "" {
		h.History.ErrorMessage = new(message)
	}

	r.Crawler.LastCrawl = &api.LastCrawlInfo{
		Status:        new(status),
		ErrorMessage:  h.History.ErrorMessage,
		StartTime:     h.History.StartTime,
		LogGroup:      h.History.LogGroup,
		LogStream:     h.History.LogStream,
		MessagePrefix: h.History.MessagePrefix,
	}

	runtime := now - common.Value(h.History.StartTime)
	r.Crawler.CrawlElapsedTime = new(int64(runtime * 1000))
	r.Crawler.State = new(stateReady)
	r.Owner = ""
	if metrics != nil {
		r.Metrics = *metrics
	} else {
		r.Metrics = api.CrawlerMetrics{}
	}

	r.Metrics.LastRuntimeSeconds = new(runtime)
	r.Runtimes = append(r.Runtimes, runtime)
	sorted := slices.Clone(r.Runtimes)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (median + sorted[len(sorted)/2-1]) / 2
	}

	r.Metrics.MedianRuntimeSeconds = new(median)
	if err := storage.Write(tx, crawlPrefix(scope, *r.Crawler.Name)+r.CrawlID, h); err != nil {
		return err
	}

	return storage.Write(tx, orchestrationKey(scope, kindCrawler, *r.Crawler.Name), r)
}

type crawlObject struct {
	classification *tabular.Classification
	key, folder    string
	size           int64
}

type crawlTable struct {
	input                  api.TableInput
	folders                map[string][]string
	objects, records, size int64
}

func customOptions(c *api.Classifier) *tabular.ClassifyOptions {
	if j := c.JsonClassifier; j != nil {
		return &tabular.ClassifyOptions{Kind: tabular.JSON, JSONPath: common.Value(j.JsonPath)}
	}

	v := c.CsvClassifier
	return &tabular.ClassifyOptions{
		Kind:            tabular.CSV,
		Delimiter:       common.Value(v.Delimiter),
		Quote:           common.Value(v.QuoteSymbol),
		ContainsHeader:  common.Value(v.ContainsHeader),
		Header:          v.Header,
		Types:           v.CustomDatatypes,
		DisableTrimming: common.Value(v.DisableValueTrimming),
		SingleColumn:    common.Value(v.AllowSingleColumn),
	}
}

func (s *Service) crawlObjects(
	ctx context.Context,
	r *crawlerRecord,
	target api.S3Target,
	custom []*api.Classifier,
) ([]crawlObject, error) {
	u, err := url.Parse(*target.Path)
	if err != nil {
		return nil, err
	}

	prefix := strings.TrimPrefix(u.Path, "/")
	var globs []*regexp.Regexp
	for _, p := range target.Exclusions {
		g, err := crawlGlob(p)
		if err != nil {
			return nil, err
		}

		globs = append(globs, g)
	}

	var out []crawlObject
	sampled := map[string]int64{}
	for token := (*string)(nil); ; {
		list, err := s.buckets.ListObjectsV2(
			ctx,
			&s3api.ListObjectsV2Request{Bucket: new(u.Host), Prefix: new(prefix), ContinuationToken: token},
		)
		if err != nil {
			return nil, err
		}

		for _, obj := range list.Contents {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			key := common.Value(obj.Key)
			base := path.Base(key)
			folder := path.Dir(key) + "/"
			if strings.HasSuffix(key, "/") || strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
				continue
			}

			excluded := false
			for _, g := range globs {
				if g.MatchString(strings.TrimPrefix(key, prefix)) {
					excluded = true
				}
			}

			if excluded {
				continue
			}

			if common.Value(r.Crawler.RecrawlPolicy.RecrawlBehavior) == recrawlNewFolders &&
				r.SeenFolders[u.Host+"/"+folder] {
				continue
			}

			if target.SampleSize != nil && sampled[folder] >= *target.SampleSize {
				continue
			}

			body, err := s.buckets.Object(ctx, u.Host, key, "")
			if err != nil {
				return nil, err
			}

			var c *tabular.Classification
			for _, classifier := range custom {
				c, err = tabular.Classify(body, customOptions(classifier))
				if err == nil {
					break
				}
			}

			if c == nil || err != nil {
				c, err = tabular.Classify(body, &tabular.ClassifyOptions{})
			}

			if err != nil {
				continue
			}

			sampled[folder]++
			out = append(out, crawlObject{key: key, folder: folder, size: common.Value(obj.Size), classification: c})
		}

		if !common.Value(list.IsTruncated) {
			break
		}

		token = list.NextContinuationToken
	}

	return out, nil
}

var crawlNameCharacters = regexp.MustCompile(`[^a-z0-9_]`)

func crawlTableName(prefix, n string) string {
	return crawlNameCharacters.ReplaceAllString(strings.ToLower(prefix+n), "_")
}

func crawlStorage(c *tabular.Classification, location string) *api.StorageDescriptor {
	sd := &api.StorageDescriptor{
		Location:     new(location),
		Compressed:   new(c.Compression == "gzip"),
		InputFormat:  new("org.apache.hadoop.mapred.TextInputFormat"),
		OutputFormat: new("org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat"),
		SerdeInfo:    &api.SerDeInfo{Parameters: map[string]string{}},
	}

	for _, col := range c.Columns {
		sd.Columns = append(sd.Columns, api.Column{Name: new(col.Name), Type: new(col.Type)})
	}

	switch c.Kind {
	case tabular.CSV:
		sd.SerdeInfo.SerializationLibrary = new("org.apache.hadoop.hive.serde2.OpenCSVSerde")
		sd.SerdeInfo.Parameters = map[string]string{"separatorChar": c.Delimiter, "quoteChar": c.Quote}
	case tabular.JSON:
		sd.SerdeInfo.SerializationLibrary = new("org.openx.data.jsonserde.JsonSerDe")
	case tabular.Parquet:
		sd.InputFormat = new("org.apache.hadoop.hive.ql.io.parquet.MapredParquetInputFormat")
		sd.OutputFormat = new("org.apache.hadoop.hive.ql.io.parquet.MapredParquetOutputFormat")
		sd.SerdeInfo.SerializationLibrary = new("org.apache.hadoop.hive.ql.io.parquet.serde.ParquetHiveSerDe")
	}

	return sd
}

func (s *Service) discoverTables(
	ctx context.Context,
	r *crawlerRecord,
	custom []*api.Classifier,
) ([]*crawlTable, error) {
	var tables []*crawlTable
	for _, target := range r.Crawler.Targets.S3Targets {
		objects, err := s.crawlObjects(ctx, r, target, custom)
		if err != nil {
			return nil, err
		}

		u, err := url.Parse(*target.Path)
		if err != nil {
			return nil, err
		}

		root := strings.Trim(strings.TrimPrefix(u.Path, "/"), "/")
		if root != "" {
			root += "/"
		}

		splitFolders := false
		if len(objects) > 1 {
			first := crawlStorage(objects[0].classification, "")
			for _, o := range objects[1:] {
				if !compatibleCrawlSchema(first, crawlStorage(o.classification, "")) {
					splitFolders = true
					break
				}
			}
		}

		for _, o := range objects {
			c := o.classification
			// Hive directories identify the table root. Compatible unnamed folders under an include path are partitions.
			tableRoot := root
			relative := strings.TrimPrefix(o.folder, root)
			dirs := strings.Split(strings.Trim(relative, "/"), "/")
			if relative == "" {
				dirs = nil
			}

			firstHive := -1
			for i, d := range dirs {
				if strings.Contains(d, "=") {
					firstHive = i
					break
				}
			}

			if firstHive > 0 {
				tableRoot += strings.Join(dirs[:firstHive], "/") + "/"
				dirs = dirs[firstHive:]
			}

			if firstHive < 0 && splitFolders && len(dirs) > 0 {
				tableRoot += dirs[0] + "/"
				dirs = dirs[1:]
			}

			var keys []api.Column
			var values []string
			for i, d := range dirs {
				k, v, ok := strings.Cut(d, "=")
				if !ok {
					k = "partition_" + strconv.Itoa(i)
					v = d
				}

				keys = append(keys, api.Column{Name: new(strings.ToLower(k)), Type: new("string")})
				values = append(values, v)
			}

			location := "s3://" + u.Host + "/" + tableRoot
			sd := crawlStorage(c, location)
			var group *crawlTable
			for _, t := range tables {
				if common.Value(t.input.StorageDescriptor.Location) == location &&
					compatibleCrawlSchema(t.input.StorageDescriptor, sd) &&
					len(t.input.PartitionKeys) == len(keys) {
					group = t
					break
				}
			}

			if group == nil {
				n := path.Base(strings.TrimSuffix(tableRoot, "/"))
				if tableRoot == "" {
					n = u.Host
				}

				n = crawlTableName(common.Value(r.Crawler.TablePrefix), n)
				// Distinct incompatible schemas need distinct catalog tables and locations.
				for _, t := range tables {
					if common.Value(t.input.Name) == n {
						return nil, invalid(
							"Incompatible schemas share S3 table root %s; use separate target paths",
							location,
						)
					}
				}

				parameters := map[string]string{
					"classification":                 c.Kind,
					"compressionType":                c.Compression,
					"typeOfData":                     "file",
					"CrawlerSchemaSerializerVersion": "1.0",
					"UPDATED_BY_CRAWLER":             *r.Crawler.Name,
				}

				if c.Header {
					parameters["skip.header.line.count"] = "1"
				}

				group = &crawlTable{
					input: api.TableInput{
						Name:              new(n),
						StorageDescriptor: sd,
						PartitionKeys:     keys,
						Parameters:        parameters,
						TableType:         new("EXTERNAL_TABLE"),
					},
					folders: map[string][]string{},
				}

				tables = append(tables, group)
			}

			group.objects++
			group.records += c.Records
			group.size += o.size
			group.folders["s3://"+u.Host+"/"+o.folder] = values
		}
	}

	for _, t := range tables {
		p := t.input.Parameters
		p["objectCount"] = strconv.FormatInt(t.objects, 10)
		p["recordCount"] = strconv.FormatInt(t.records, 10)
		p["sizeKey"] = strconv.FormatInt(t.size, 10)
		average := int64(0)
		if t.records > 0 {
			average = t.size / t.records
		}

		p["averageRecordSize"] = strconv.FormatInt(average, 10)
	}

	return tables, nil
}

func compatibleCrawlSchema(a, b *api.StorageDescriptor) bool {
	if common.Value(a.InputFormat) != common.Value(b.InputFormat) ||
		common.Value(a.SerdeInfo.SerializationLibrary) != common.Value(b.SerdeInfo.SerializationLibrary) ||
		a.SerdeInfo.Parameters["separatorChar"] != b.SerdeInfo.Parameters["separatorChar"] ||
		len(a.Columns) != len(b.Columns) {
		return false
	}

	for i, c := range a.Columns {
		if common.Value(c.Name) != common.Value(b.Columns[i].Name) ||
			common.Value(c.Type) != common.Value(b.Columns[i].Type) {
			return false
		}
	}

	return true
}

func (s *Service) executeCrawl(parent context.Context, scope request.Scope, r *crawlerRecord) {
	ctx := request.WithMetadata(parent, r.Metadata)
	var custom []*api.Classifier
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, n := range r.Crawler.Classifiers {
			c, err := loadClassifier(tx, scope, n)
			if err != nil {
				return err
			}

			custom = append(custom, c)
		}

		return nil
	})

	var tables []*crawlTable
	if err == nil {
		tables, err = s.discoverTables(ctx, r, custom)
	}

	if parent.Err() != nil {
		return
	}

	status, message := stateSucceeded, ""
	if err != nil {
		status = stateFailed
		message = err.Error()
	}

	if logErr := s.logs.Append(
		ctx,
		scope,
		"/aws-glue/crawlers",
		*r.Crawler.Name,
		[]string{r.CrawlID + " " + status + " " + message},
	); logErr != nil {
		status = stateFailed
		message = logErr.Error()
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, err := loadCrawler(tx, scope, *r.Crawler.Name)
		if err != nil {
			return err
		}

		if current.CrawlID != r.CrawlID || current.Owner != s.session {
			return nil
		}

		metrics := &api.CrawlerMetrics{
			TablesCreated: new(int64(0)),
			TablesUpdated: new(int64(0)),
			TablesDeleted: new(int64(0)),
		}

		if status == stateSucceeded && common.Value(current.Crawler.State) != stateStopping {
			if err := s.applyCrawl(tx, scope, current, tables, metrics); err != nil {
				return err
			}
		}

		return s.finishCrawl(tx, scope, current, status, message, metrics)
	})
}

func (s *Service) applyCrawl(
	tx storage.WriteTx,
	scope request.Scope,
	r *crawlerRecord,
	tables []*crawlTable,
	m *api.CrawlerMetrics,
) error {
	db := *r.Crawler.DatabaseName
	now := common.Unix(s.now())
	if _, ok := tx.Get(databaseKey(scope, db)); !ok {
		if err := storage.Write(
			tx,
			databaseKey(scope, db),
			database{Database: api.Database{Name: new(db), CatalogId: new(scope.AccountID), CreateTime: new(now)}},
		); err != nil {
			return err
		}
	}

	found := map[string]bool{}
	for _, t := range tables {
		n := *t.input.Name
		if _, err := name("table", new(n)); err != nil {
			return err
		}

		found[n] = true
		old, ok, err := storage.Read[api.Table](tx, tableKey(scope, db, n))
		if err != nil {
			return err
		}

		created, version := now, 0
		write := !ok
		if ok {
			created = common.Value(old.CreateTime)
			version, _ = strconv.Atoi(common.Value(old.VersionId))
			version++
			write = common.Value(r.Crawler.SchemaChangePolicy.UpdateBehavior) == "UPDATE_IN_DATABASE"
		}

		if write {
			if err := storage.Write(
				tx,
				tableKey(scope, db, n),
				tableOf(scope, db, n, &t.input, created, now, version),
			); err != nil {
				return err
			}

			if ok {
				*m.TablesUpdated++
			} else {
				*m.TablesCreated++
			}
		}

		if common.Value(r.Crawler.RecrawlPolicy.RecrawlBehavior) == "CRAWL_EVERYTHING" {
			for _, key := range tx.Keys(partitionPrefix(scope, db, n)) {
				partition, ok, err := storage.Read[api.Partition](tx, key)
				if err != nil {
					return err
				}

				if !ok || partition.StorageDescriptor == nil {
					continue
				}

				if _, exists := t.folders[common.Value(partition.StorageDescriptor.Location)]; exists {
					continue
				}

				switch common.Value(r.Crawler.SchemaChangePolicy.DeleteBehavior) {
				case deleteFromDatabase:
					tx.Delete(key)
				case deprecateInDatabase:
					if partition.Parameters == nil {
						partition.Parameters = map[string]string{}
					}

					partition.Parameters["DEPRECATED_BY_CRAWLER"] = strconv.FormatFloat(now, 'f', -1, 64)
					if err := storage.Write(tx, key, partition); err != nil {
						return err
					}
				}
			}
		}

		for location, values := range t.folders {
			r.SeenFolders[strings.TrimPrefix(location, "s3://")] = true
			if len(values) == 0 {
				continue
			}

			key := partitionKey(scope, db, n, values)
			if _, exists := tx.Get(key); exists && !write {
				continue
			}

			sd := common.Clone(t.input.StorageDescriptor)
			sd.Location = new(location)
			p := api.Partition{
				DatabaseName:      new(db),
				TableName:         new(n),
				CatalogId:         new(scope.AccountID),
				CreationTime:      new(now),
				Values:            values,
				StorageDescriptor: sd,
			}

			if err := storage.Write(tx, key, p); err != nil {
				return err
			}
		}
	}

	if common.Value(r.Crawler.RecrawlPolicy.RecrawlBehavior) == recrawlNewFolders {
		return nil
	}

	existing, err := storage.Records[api.Table](tx, scope.Key(service, kindTable, db+"/"))
	if err != nil {
		return err
	}

	for _, t := range existing {
		if t.Parameters["UPDATED_BY_CRAWLER"] != *r.Crawler.Name || found[*t.Name] {
			continue
		}

		switch common.Value(r.Crawler.SchemaChangePolicy.DeleteBehavior) {
		case deleteFromDatabase:
			deleteTable(tx, scope, db, *t.Name)
			*m.TablesDeleted++
		case deprecateInDatabase:
			t.Parameters["DEPRECATED_BY_CRAWLER"] = strconv.FormatFloat(now, 'f', -1, 64)
			if err := storage.Write(tx, tableKey(scope, db, *t.Name), t); err != nil {
				return err
			}
		}
	}

	return nil
}
