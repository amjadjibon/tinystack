package emr

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RunJobFlow(ctx context.Context, in *api.RunJobFlowInput) (*api.RunJobFlowOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(common.Value(in.Name)) == "" || strings.ContainsAny(common.Value(in.Name), "<>$|`") {
		return nil, invalid("Name is required and cannot contain < > $ | or backticks.")
	}

	if !slices.Contains(releases, common.Value(in.ReleaseLabel)) {
		return nil, invalid("Unsupported ReleaseLabel.")
	}

	if in.Instances == nil {
		return nil, invalid("Instances is required.")
	}

	if in.StepConcurrencyLevel == nil {
		in.StepConcurrencyLevel = new(int64(1))
	}

	if concurrencyErr := concurrency(in.StepConcurrencyLevel); concurrencyErr != nil {
		return nil, concurrencyErr
	}

	if in.AutoTerminationPolicy != nil && (common.Value(in.AutoTerminationPolicy.IdleTimeout) < 60 ||
		common.Value(in.AutoTerminationPolicy.IdleTimeout) > 604800) {
		return nil, invalid("IdleTimeout must be 60 to 604800 seconds.")
	}

	if referenceErr := s.references(ctx, scope, in); referenceErr != nil {
		return nil, referenceErr
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	id := "j-" + strings.ToUpper(common.RandomID(13))
	arn := "arn:aws:elasticmapreduce:" + scope.Region + ":" + scope.AccountID + ":cluster/" + id
	r := &record{Cluster: api.Cluster{
		Id:             new(id),
		Name:           in.Name,
		ReleaseLabel:   in.ReleaseLabel,
		Applications:   in.Applications,
		Configurations: in.Configurations,
		LogUri:         in.LogUri,
		ServiceRole:    in.ServiceRole,
		Tags:           tagList(tags),
		VisibleToAllUsers: common.OrElse(
			in.VisibleToAllUsers,
			new(true),
		),
		TerminationProtected:     in.Instances.TerminationProtected,
		UnhealthyNodeReplacement: in.Instances.UnhealthyNodeReplacement,
		AutoTerminate: new(
			!common.Value(in.Instances.KeepJobFlowAliveWhenNoSteps),
		),
		StepConcurrencyLevel:  in.StepConcurrencyLevel,
		SecurityConfiguration: in.SecurityConfiguration,
		ClusterArn:            new(arn),
		Status: &api.ClusterStatus{State: new("STARTING"), StateChangeReason: &api.ClusterStateChangeReason{
			Code: new(userRequest), Message: new(simulated),
		}, Timeline: &api.ClusterTimeline{CreationDateTime: common.SecondsMilli(time.Now())}},
	}}
	if in.AutoTerminationPolicy != nil {
		r.IdleTimeout = common.Value(in.AutoTerminationPolicy.IdleTimeout)
	}

	for _, b := range in.BootstrapActions {
		if b.Name == nil || b.ScriptBootstrapAction == nil || common.Value(b.ScriptBootstrapAction.Path) == "" {
			return nil, invalid("Bootstrap actions require a name and script path.")
		}

		r.Bootstrap = append(
			r.Bootstrap,
			api.Command{Name: b.Name, ScriptPath: b.ScriptBootstrapAction.Path, Args: b.ScriptBootstrapAction.Args},
		)
	}

	if len(in.Instances.InstanceGroups) > 0 && len(in.Instances.InstanceFleets) > 0 {
		return nil, invalid("InstanceGroups and InstanceFleets are mutually exclusive.")
	}

	if len(in.Instances.InstanceGroups) == 0 && len(in.Instances.InstanceFleets) == 0 {
		return nil, invalid("An instance group or fleet is required.")
	}

	r.Cluster.InstanceCollectionType = new(instanceGroups)
	if len(in.Instances.InstanceFleets) > 0 {
		r.Cluster.InstanceCollectionType = new(instanceFleets)
	}

	if _, err = addGroups(r, in.Instances.InstanceGroups, false); err != nil {
		return nil, err
	}

	for _, f := range in.Instances.InstanceFleets {
		if _, err = addFleet(r, &f, false); err != nil {
			return nil, err
		}
	}

	if !hasMaster(r) {
		return nil, invalid("Exactly one MASTER group or fleet is required.")
	}

	if _, err = addSteps(r, in.Steps); err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.SecurityConfiguration != nil {
			if _, ok, e := storage.Read[api.DescribeSecurityConfigurationOutput](
				tx,
				scope.Key(serviceName, "securityconfiguration", *in.SecurityConfiguration),
			); e != nil {
				return e
			} else if !ok {
				return invalid("SecurityConfiguration does not exist.")
			}
		}

		var e error
		r.CIDR, r.Cluster.Ec2InstanceAttributes, e = s.networkReferences(tx, scope, in.Instances)
		if e != nil {
			return e
		}

		r.Cluster.Ec2InstanceAttributes.IamInstanceProfile = in.JobFlowRole
		return save(tx, scope, r)
	})
	return &api.RunJobFlowOutput{JobFlowId: new(id), ClusterArn: new(arn)}, err
}

func (s *Service) DescribeCluster(
	ctx context.Context,
	in *api.DescribeClusterInput,
) (*api.DescribeClusterOutput, error) {
	r, err := s.change(ctx, in.ClusterId, func(r *record) error { advance(r); return nil })
	if err != nil {
		return nil, err
	}

	return &api.DescribeClusterOutput{Cluster: &r.Cluster}, nil
}

func (s *Service) ListClusters(ctx context.Context, in *api.ListClustersInput) (*api.ListClustersOutput, error) {
	if err := checkStates(
		in.ClusterStates,
		strings.Fields("STARTING BOOTSTRAPPING RUNNING WAITING TERMINATING TERMINATED TERMINATED_WITH_ERRORS"),
	); err != nil {
		return nil, err
	}

	out := &api.ListClustersOutput{}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, "cluster"))
		if e != nil {
			return e
		}

		for _, r := range rows {
			advance(r)
			if saveErr := save(tx, scope, r); saveErr != nil {
				return saveErr
			}

			c := r.Cluster
			created := common.Value(c.Status.Timeline.CreationDateTime)
			if len(in.ClusterStates) > 0 && !slices.Contains(in.ClusterStates, *c.Status.State) {
				continue
			}

			if in.CreatedAfter != nil && created <= *in.CreatedAfter ||
				in.CreatedBefore != nil && created >= *in.CreatedBefore {
				continue
			}

			out.Clusters = append(
				out.Clusters,
				api.ClusterSummary{Id: c.Id, Name: c.Name, Status: c.Status, ClusterArn: c.ClusterArn},
			)
		}

		out.Clusters, out.Marker, e = common.Page(
			out.Clusters,
			func(c api.ClusterSummary) string { return *c.Id },
			in.Marker,
			50,
			invalid("Invalid Marker."),
		)
		return e
	})
	return out, err
}

func (s *Service) many(ctx context.Context, ids []string, fn func(*record) error) error {
	if len(ids) == 0 || len(ids) > 10 {
		return invalid("Supply 1 to 10 JobFlowIds.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, id := range ids {
			r, e := read(tx, scope, new(id))
			if e != nil {
				return e
			}

			if e := fn(r); e != nil {
				return e
			}

			if saveErr := save(tx, scope, r); saveErr != nil {
				return saveErr
			}
		}

		return nil
	})
}

func (s *Service) TerminateJobFlows(
	ctx context.Context,
	in *api.TerminateJobFlowsInput,
) (*api.TerminateJobFlowsOutput, error) {
	err := s.many(ctx, in.JobFlowIds, func(r *record) error {
		if common.Value(r.Cluster.TerminationProtected) {
			return invalid("Cluster is termination protected.")
		}

		if !terminal(r) {
			state(r, terminating, userRequest)
		}

		return nil
	})
	return &api.TerminateJobFlowsOutput{}, err
}

func (s *Service) SetTerminationProtection(
	ctx context.Context,
	in *api.SetTerminationProtectionInput,
) (*api.SetTerminationProtectionOutput, error) {
	if in.TerminationProtected == nil {
		return nil, invalid("TerminationProtected is required.")
	}

	err := s.many(ctx, in.JobFlowIds, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		r.Cluster.TerminationProtected = in.TerminationProtected
		return nil
	})
	return &api.SetTerminationProtectionOutput{}, err
}

func (s *Service) SetVisibleToAllUsers(
	ctx context.Context,
	in *api.SetVisibleToAllUsersInput,
) (*api.SetVisibleToAllUsersOutput, error) {
	if in.VisibleToAllUsers == nil {
		return nil, invalid("VisibleToAllUsers is required.")
	}

	err := s.many(ctx, in.JobFlowIds, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		r.Cluster.VisibleToAllUsers = in.VisibleToAllUsers
		return nil
	})
	return &api.SetVisibleToAllUsersOutput{}, err
}

func (s *Service) ModifyCluster(ctx context.Context, in *api.ModifyClusterInput) (*api.ModifyClusterOutput, error) {
	if err := concurrency(in.StepConcurrencyLevel); err != nil {
		return nil, err
	}

	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		r.Cluster.StepConcurrencyLevel = in.StepConcurrencyLevel
		return nil
	})
	return &api.ModifyClusterOutput{StepConcurrencyLevel: in.StepConcurrencyLevel}, err
}

func (s *Service) GetAutoTerminationPolicy(
	ctx context.Context,
	in *api.GetAutoTerminationPolicyInput,
) (*api.GetAutoTerminationPolicyOutput, error) {
	out := &api.GetAutoTerminationPolicyOutput{}
	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if r.IdleTimeout > 0 {
			out.AutoTerminationPolicy = &api.AutoTerminationPolicy{IdleTimeout: new(r.IdleTimeout)}
		}

		return nil
	})
	return out, err
}
