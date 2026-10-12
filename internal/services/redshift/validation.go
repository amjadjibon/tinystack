package redshift

import (
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/common"
)

var (
	namePattern          = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	subnetNamePattern    = regexp.MustCompile(`^[a-z0-9-]+$`)
	usernamePattern      = regexp.MustCompile(`^[a-z][a-z0-9_+.@-]*$`)
	databasePattern      = regexp.MustCompile(`^[a-z0-9]+$`)
	securityGroupPattern = regexp.MustCompile(`^sg-([0-9a-f]{8}|[0-9a-f]{17})$`)
)

func validName(name string, maximum int) bool {
	return len(name) >= 1 && len(name) <= maximum && namePattern.MatchString(name) &&
		!strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
}

func checkPassword(password *string) error {
	value := common.Value(password)
	if len(value) < 8 || len(value) > 64 {
		return invalid("MasterUserPassword must contain 8 to 64 characters")
	}

	upper, lower, number := false, false, false
	for _, c := range value {
		if c < 33 || c > 126 || strings.ContainsRune("'\"\\/@", c) {
			return invalid("MasterUserPassword contains a prohibited character")
		}

		upper = upper || c >= 'A' && c <= 'Z'
		lower = lower || c >= 'a' && c <= 'z'
		number = number || c >= '0' && c <= '9'
	}

	if !upper || !lower || !number {
		return invalid("MasterUserPassword requires uppercase, lowercase and numeric characters")
	}

	return nil
}

func checkCluster(r *record) error {
	c := r.Cluster
	if !validName(r.Name, 63) {
		return invalid("Invalid ClusterIdentifier")
	}

	user, db := common.Value(c.MasterUsername), common.Value(c.DBName)
	if len(user) > 128 || !usernamePattern.MatchString(user) || user == "public" || reserved(user) {
		return invalid("Invalid MasterUsername")
	}

	if len(db) > 64 || !databasePattern.MatchString(db) || reserved(db) {
		return invalid("Invalid DBName")
	}

	node := common.Value(c.NodeType)
	if !slices.Contains([]string{
		"dc2.large", "dc2.8xlarge", "rg.large", "rg.xlarge", "rg.4xlarge", "rg.12xlarge",
		"ra3.large", "ra3.xlplus", "ra3.4xlarge", "ra3.16xlarge",
	}, node) {
		return invalid("Invalid NodeType")
	}

	n := common.Value(c.NumberOfNodes)
	if n < 1 || n > 100 || (r.ClusterType == singleNode && n != 1) ||
		(r.ClusterType == multiNode && n < 2) || (r.ClusterType != singleNode && r.ClusterType != multiNode) {
		return invalid("Invalid ClusterType or NumberOfNodes")
	}

	port := common.Value(c.Endpoint.Port)
	if strings.HasPrefix(node, "dc2.") {
		if port < 1150 || port > 65535 {
			return invalid("Port must be between 1150 and 65535 for DC2 nodes")
		}
	} else if (port < 5431 || port > 5455) && (port < 8191 || port > 8215) {
		return invalid("Invalid Port for RG or RA3 nodes")
	}

	days := common.Value(c.AutomatedSnapshotRetentionPeriod)
	if days < 0 || days > 35 || (!strings.HasPrefix(node, "dc2.") && days == 0) {
		return invalid("Invalid AutomatedSnapshotRetentionPeriod")
	}

	for _, g := range c.VpcSecurityGroups {
		if !securityGroupPattern.MatchString(common.Value(g.VpcSecurityGroupId)) {
			return invalid("Invalid VpcSecurityGroupId")
		}
	}

	return nil
}

func pageSize(maximum *int64) (int, error) {
	n := common.Value(common.OrElse(maximum, new(int64(100))))
	if n < 20 || n > 100 {
		return 0, invalid("MaxRecords must be between 20 and 100")
	}

	return int(n), nil
}

func memberships(ids []string) []api.VpcSecurityGroupMembership {
	out := make([]api.VpcSecurityGroupMembership, 0, len(ids))
	for _, id := range ids {
		out = append(out, api.VpcSecurityGroupMembership{VpcSecurityGroupId: new(id), Status: new("active")})
	}

	return out
}

// Reserved identifiers from the Redshift Database Developer Guide.
// https://docs.aws.amazon.com/redshift/latest/dg/r_pg_keywords.html
func reserved(name string) bool {
	words := " aes128 aes256 all allowoverwrite analyse analyze and any array as asc authorization az64 " +
		"backup between binary blanksasnull both bytedict bzip2 case cast check collate column " +
		"constraint create credentials cross current_date current_time current_timestamp " +
		"current_user current_user_id default deferrable deflate defrag delta delta32k desc disable " +
		"distinct do else emptyasnull enable encode encrypt encryption end except explicit false " +
		"for foreign freeze from full globaldict256 globaldict64k grant group gzip having identity " +
		"ignore ilike in initially inner intersect interval into is isnull join leading left like " +
		"limit localtime localtimestamp lun luns lzo lzop minus mostly16 mostly32 mostly8 natural " +
		"new not notnull null nulls off offline offset oid old on only open or order outer overlaps " +
		"parallel partition percent permissions pivot placing primary raw readratio recover " +
		"references rejectlog resort respect restore right select session_user similar snapshot " +
		"some sysdate system table tag tdes text255 text32k then timestamp to top trailing true " +
		"truncatecolumns union unique unnest unpivot user using verbose wallet when where with " +
		"without "
	return strings.Contains(words, " "+name+" ")
}
