package opensearchserverless

import api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"

func createCollectionDetail(r *api.CollectionDetail) *api.CreateCollectionDetail {
	return &api.CreateCollectionDetail{
		Id:               r.Id,
		Name:             r.Name,
		Status:           r.Status,
		Type:             r.Type,
		Description:      r.Description,
		Arn:              r.Arn,
		KmsKeyArn:        r.KmsKeyArn,
		StandbyReplicas:  r.StandbyReplicas,
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}

func updateCollectionDetail(r *api.CollectionDetail) *api.UpdateCollectionDetail {
	return &api.UpdateCollectionDetail{
		Id:               r.Id,
		Name:             r.Name,
		Status:           r.Status,
		Type:             r.Type,
		Description:      r.Description,
		Arn:              r.Arn,
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}

func deleteCollectionDetail(r *api.CollectionDetail) *api.DeleteCollectionDetail {
	return &api.DeleteCollectionDetail{Id: r.Id, Name: r.Name, Status: r.Status}
}

func collectionSummary(r *api.CollectionDetail) *api.CollectionSummary {
	return &api.CollectionSummary{Id: r.Id, Name: r.Name, Status: r.Status, Arn: r.Arn, KmsKeyArn: r.KmsKeyArn}
}

func securityPolicyDetail(r *policy) *api.SecurityPolicyDetail {
	return &api.SecurityPolicyDetail{
		Type:             r.Type,
		Name:             r.Name,
		PolicyVersion:    r.PolicyVersion,
		Description:      r.Description,
		Policy:           new(r.Policy),
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}

func accessPolicyDetail(r *policy) *api.AccessPolicyDetail {
	return &api.AccessPolicyDetail{
		Type:             r.Type,
		Name:             r.Name,
		PolicyVersion:    r.PolicyVersion,
		Description:      r.Description,
		Policy:           new(r.Policy),
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}

func securityPolicySummary(r *policy) *api.SecurityPolicySummary {
	return &api.SecurityPolicySummary{
		Type:             r.Type,
		Name:             r.Name,
		PolicyVersion:    r.PolicyVersion,
		Description:      r.Description,
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}

func accessPolicySummary(r *policy) *api.AccessPolicySummary {
	return &api.AccessPolicySummary{
		Type:             r.Type,
		Name:             r.Name,
		PolicyVersion:    r.PolicyVersion,
		Description:      r.Description,
		CreatedDate:      r.CreatedDate,
		LastModifiedDate: r.LastModifiedDate,
	}
}
