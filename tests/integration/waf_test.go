package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/waf"
	"github.com/aws/aws-sdk-go-v2/service/waf/types"
	"github.com/aws/smithy-go"
)

func wafClassicClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func classicToken(ctx context.Context, t *testing.T, c *sdk.Client) *string {
	t.Helper()
	out, err := c.GetChangeToken(ctx, &sdk.GetChangeTokenInput{})
	if err != nil {
		t.Fatal(err)
	}

	return out.ChangeToken
}

func classicError(t *testing.T, err error, code string) {
	t.Helper()
	var api smithy.APIError
	if !errors.As(err, &api) || api.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestWAFClassicConditions(t *testing.T) {
	c := wafClassicClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	pattern, err := c.CreateRegexPatternSet(
		ctx,
		&sdk.CreateRegexPatternSetInput{Name: new("dependency"), ChangeToken: classicToken(ctx, t, c)},
	)
	if err != nil {
		t.Fatal(err)
	}

	patternID := pattern.RegexPatternSet.RegexPatternSetId
	t.Run("IPSet", func(t *testing.T) {
		created, err := c.CreateIPSet(
			ctx,
			&sdk.CreateIPSetInput{Name: new("IPSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.IPSet.IPSetId
		item := types.IPSetDescriptor{Type: types.IPSetDescriptorTypeIpv4, Value: new("192.0.2.0/24")}
		update := types.IPSetUpdate{Action: types.ChangeActionInsert, IPSetDescriptor: &item}
		_, err = c.UpdateIPSet(
			ctx,
			&sdk.UpdateIPSetInput{
				IPSetId:     id,
				ChangeToken: classicToken(ctx, t, c),
				Updates:     []types.IPSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetIPSet(ctx, &sdk.GetIPSetInput{IPSetId: id})
		if err != nil || len(got.IPSet.IPSetDescriptors) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListIPSets(ctx, &sdk.ListIPSetsInput{Limit: 100})
		if err != nil || len(listed.IPSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteIPSet(ctx, &sdk.DeleteIPSetInput{IPSetId: id, ChangeToken: classicToken(ctx, t, c)})
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateIPSet(
			ctx,
			&sdk.UpdateIPSetInput{
				IPSetId:     id,
				ChangeToken: classicToken(ctx, t, c),
				Updates:     []types.IPSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateIPSet(
			ctx,
			&sdk.UpdateIPSetInput{
				IPSetId:     id,
				ChangeToken: classicToken(ctx, t, c),
				Updates:     []types.IPSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateIPSet(
			ctx,
			&sdk.UpdateIPSetInput{
				IPSetId:     id,
				ChangeToken: classicToken(ctx, t, c),
				Updates:     []types.IPSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteIPSet(ctx, &sdk.DeleteIPSetInput{IPSetId: id, ChangeToken: classicToken(ctx, t, c)})
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetIPSet(ctx, &sdk.GetIPSetInput{IPSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("ByteMatchSet", func(t *testing.T) {
		created, err := c.CreateByteMatchSet(
			ctx,
			&sdk.CreateByteMatchSetInput{Name: new("ByteMatchSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.ByteMatchSet.ByteMatchSetId
		item := types.ByteMatchTuple{
			FieldToMatch:         &types.FieldToMatch{Type: types.MatchFieldTypeUri},
			TextTransformation:   types.TextTransformationNone,
			PositionalConstraint: types.PositionalConstraintContains,
			TargetString:         []byte("hello"),
		}

		update := types.ByteMatchSetUpdate{Action: types.ChangeActionInsert, ByteMatchTuple: &item}
		_, err = c.UpdateByteMatchSet(
			ctx,
			&sdk.UpdateByteMatchSetInput{
				ByteMatchSetId: id,
				ChangeToken:    classicToken(ctx, t, c),
				Updates:        []types.ByteMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetByteMatchSet(ctx, &sdk.GetByteMatchSetInput{ByteMatchSetId: id})
		if err != nil || len(got.ByteMatchSet.ByteMatchTuples) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListByteMatchSets(ctx, &sdk.ListByteMatchSetsInput{Limit: 100})
		if err != nil || len(listed.ByteMatchSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteByteMatchSet(
			ctx,
			&sdk.DeleteByteMatchSetInput{ByteMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateByteMatchSet(
			ctx,
			&sdk.UpdateByteMatchSetInput{
				ByteMatchSetId: id,
				ChangeToken:    classicToken(ctx, t, c),
				Updates:        []types.ByteMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateByteMatchSet(
			ctx,
			&sdk.UpdateByteMatchSetInput{
				ByteMatchSetId: id,
				ChangeToken:    classicToken(ctx, t, c),
				Updates:        []types.ByteMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateByteMatchSet(
			ctx,
			&sdk.UpdateByteMatchSetInput{
				ByteMatchSetId: id,
				ChangeToken:    classicToken(ctx, t, c),
				Updates:        []types.ByteMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteByteMatchSet(
			ctx,
			&sdk.DeleteByteMatchSetInput{ByteMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetByteMatchSet(ctx, &sdk.GetByteMatchSetInput{ByteMatchSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("SizeConstraintSet", func(t *testing.T) {
		created, err := c.CreateSizeConstraintSet(
			ctx,
			&sdk.CreateSizeConstraintSetInput{Name: new("SizeConstraintSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.SizeConstraintSet.SizeConstraintSetId
		item := types.SizeConstraint{
			FieldToMatch:       &types.FieldToMatch{Type: types.MatchFieldTypeBody},
			TextTransformation: types.TextTransformationNone,
			ComparisonOperator: types.ComparisonOperatorGt,
			Size:               3,
		}

		update := types.SizeConstraintSetUpdate{Action: types.ChangeActionInsert, SizeConstraint: &item}
		_, err = c.UpdateSizeConstraintSet(
			ctx,
			&sdk.UpdateSizeConstraintSetInput{
				SizeConstraintSetId: id,
				ChangeToken:         classicToken(ctx, t, c),
				Updates:             []types.SizeConstraintSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetSizeConstraintSet(ctx, &sdk.GetSizeConstraintSetInput{SizeConstraintSetId: id})
		if err != nil || len(got.SizeConstraintSet.SizeConstraints) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListSizeConstraintSets(ctx, &sdk.ListSizeConstraintSetsInput{Limit: 100})
		if err != nil || len(listed.SizeConstraintSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteSizeConstraintSet(
			ctx,
			&sdk.DeleteSizeConstraintSetInput{SizeConstraintSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateSizeConstraintSet(
			ctx,
			&sdk.UpdateSizeConstraintSetInput{
				SizeConstraintSetId: id,
				ChangeToken:         classicToken(ctx, t, c),
				Updates:             []types.SizeConstraintSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateSizeConstraintSet(
			ctx,
			&sdk.UpdateSizeConstraintSetInput{
				SizeConstraintSetId: id,
				ChangeToken:         classicToken(ctx, t, c),
				Updates:             []types.SizeConstraintSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateSizeConstraintSet(
			ctx,
			&sdk.UpdateSizeConstraintSetInput{
				SizeConstraintSetId: id,
				ChangeToken:         classicToken(ctx, t, c),
				Updates:             []types.SizeConstraintSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteSizeConstraintSet(
			ctx,
			&sdk.DeleteSizeConstraintSetInput{SizeConstraintSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetSizeConstraintSet(ctx, &sdk.GetSizeConstraintSetInput{SizeConstraintSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("SqlInjectionMatchSet", func(t *testing.T) {
		created, err := c.CreateSqlInjectionMatchSet(
			ctx,
			&sdk.CreateSqlInjectionMatchSetInput{
				Name:        new("SqlInjectionMatchSet"),
				ChangeToken: classicToken(ctx, t, c),
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.SqlInjectionMatchSet.SqlInjectionMatchSetId
		item := types.SqlInjectionMatchTuple{
			FieldToMatch:       &types.FieldToMatch{Type: types.MatchFieldTypeQueryString},
			TextTransformation: types.TextTransformationUrlDecode,
		}

		update := types.SqlInjectionMatchSetUpdate{Action: types.ChangeActionInsert, SqlInjectionMatchTuple: &item}
		_, err = c.UpdateSqlInjectionMatchSet(
			ctx,
			&sdk.UpdateSqlInjectionMatchSetInput{
				SqlInjectionMatchSetId: id,
				ChangeToken:            classicToken(ctx, t, c),
				Updates:                []types.SqlInjectionMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetSqlInjectionMatchSet(ctx, &sdk.GetSqlInjectionMatchSetInput{SqlInjectionMatchSetId: id})
		if err != nil || len(got.SqlInjectionMatchSet.SqlInjectionMatchTuples) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListSqlInjectionMatchSets(ctx, &sdk.ListSqlInjectionMatchSetsInput{Limit: 100})
		if err != nil || len(listed.SqlInjectionMatchSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteSqlInjectionMatchSet(
			ctx,
			&sdk.DeleteSqlInjectionMatchSetInput{SqlInjectionMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateSqlInjectionMatchSet(
			ctx,
			&sdk.UpdateSqlInjectionMatchSetInput{
				SqlInjectionMatchSetId: id,
				ChangeToken:            classicToken(ctx, t, c),
				Updates:                []types.SqlInjectionMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateSqlInjectionMatchSet(
			ctx,
			&sdk.UpdateSqlInjectionMatchSetInput{
				SqlInjectionMatchSetId: id,
				ChangeToken:            classicToken(ctx, t, c),
				Updates:                []types.SqlInjectionMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateSqlInjectionMatchSet(
			ctx,
			&sdk.UpdateSqlInjectionMatchSetInput{
				SqlInjectionMatchSetId: id,
				ChangeToken:            classicToken(ctx, t, c),
				Updates:                []types.SqlInjectionMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteSqlInjectionMatchSet(
			ctx,
			&sdk.DeleteSqlInjectionMatchSetInput{SqlInjectionMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetSqlInjectionMatchSet(ctx, &sdk.GetSqlInjectionMatchSetInput{SqlInjectionMatchSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("XssMatchSet", func(t *testing.T) {
		created, err := c.CreateXssMatchSet(
			ctx,
			&sdk.CreateXssMatchSetInput{Name: new("XssMatchSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.XssMatchSet.XssMatchSetId
		item := types.XssMatchTuple{
			FieldToMatch:       &types.FieldToMatch{Type: types.MatchFieldTypeBody},
			TextTransformation: types.TextTransformationNone,
		}

		update := types.XssMatchSetUpdate{Action: types.ChangeActionInsert, XssMatchTuple: &item}
		_, err = c.UpdateXssMatchSet(
			ctx,
			&sdk.UpdateXssMatchSetInput{
				XssMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.XssMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetXssMatchSet(ctx, &sdk.GetXssMatchSetInput{XssMatchSetId: id})
		if err != nil || len(got.XssMatchSet.XssMatchTuples) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListXssMatchSets(ctx, &sdk.ListXssMatchSetsInput{Limit: 100})
		if err != nil || len(listed.XssMatchSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteXssMatchSet(
			ctx,
			&sdk.DeleteXssMatchSetInput{XssMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateXssMatchSet(
			ctx,
			&sdk.UpdateXssMatchSetInput{
				XssMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.XssMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateXssMatchSet(
			ctx,
			&sdk.UpdateXssMatchSetInput{
				XssMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.XssMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateXssMatchSet(
			ctx,
			&sdk.UpdateXssMatchSetInput{
				XssMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.XssMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteXssMatchSet(
			ctx,
			&sdk.DeleteXssMatchSetInput{XssMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetXssMatchSet(ctx, &sdk.GetXssMatchSetInput{XssMatchSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("GeoMatchSet", func(t *testing.T) {
		created, err := c.CreateGeoMatchSet(
			ctx,
			&sdk.CreateGeoMatchSetInput{Name: new("GeoMatchSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.GeoMatchSet.GeoMatchSetId
		item := types.GeoMatchConstraint{
			Type:  types.GeoMatchConstraintTypeCountry,
			Value: types.GeoMatchConstraintValueUs,
		}

		update := types.GeoMatchSetUpdate{Action: types.ChangeActionInsert, GeoMatchConstraint: &item}
		_, err = c.UpdateGeoMatchSet(
			ctx,
			&sdk.UpdateGeoMatchSetInput{
				GeoMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.GeoMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetGeoMatchSet(ctx, &sdk.GetGeoMatchSetInput{GeoMatchSetId: id})
		if err != nil || len(got.GeoMatchSet.GeoMatchConstraints) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListGeoMatchSets(ctx, &sdk.ListGeoMatchSetsInput{Limit: 100})
		if err != nil || len(listed.GeoMatchSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteGeoMatchSet(
			ctx,
			&sdk.DeleteGeoMatchSetInput{GeoMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateGeoMatchSet(
			ctx,
			&sdk.UpdateGeoMatchSetInput{
				GeoMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.GeoMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateGeoMatchSet(
			ctx,
			&sdk.UpdateGeoMatchSetInput{
				GeoMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.GeoMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateGeoMatchSet(
			ctx,
			&sdk.UpdateGeoMatchSetInput{
				GeoMatchSetId: id,
				ChangeToken:   classicToken(ctx, t, c),
				Updates:       []types.GeoMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteGeoMatchSet(
			ctx,
			&sdk.DeleteGeoMatchSetInput{GeoMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetGeoMatchSet(ctx, &sdk.GetGeoMatchSetInput{GeoMatchSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("RegexPatternSet", func(t *testing.T) {
		created, err := c.CreateRegexPatternSet(
			ctx,
			&sdk.CreateRegexPatternSetInput{Name: new("RegexPatternSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.RegexPatternSet.RegexPatternSetId
		item := "hello.*"
		update := types.RegexPatternSetUpdate{Action: types.ChangeActionInsert, RegexPatternString: new(item)}
		_, err = c.UpdateRegexPatternSet(
			ctx,
			&sdk.UpdateRegexPatternSetInput{
				RegexPatternSetId: id,
				ChangeToken:       classicToken(ctx, t, c),
				Updates:           []types.RegexPatternSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetRegexPatternSet(ctx, &sdk.GetRegexPatternSetInput{RegexPatternSetId: id})
		if err != nil || len(got.RegexPatternSet.RegexPatternStrings) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListRegexPatternSets(ctx, &sdk.ListRegexPatternSetsInput{Limit: 100})
		if err != nil || len(listed.RegexPatternSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteRegexPatternSet(
			ctx,
			&sdk.DeleteRegexPatternSetInput{RegexPatternSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateRegexPatternSet(
			ctx,
			&sdk.UpdateRegexPatternSetInput{
				RegexPatternSetId: id,
				ChangeToken:       classicToken(ctx, t, c),
				Updates:           []types.RegexPatternSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateRegexPatternSet(
			ctx,
			&sdk.UpdateRegexPatternSetInput{
				RegexPatternSetId: id,
				ChangeToken:       classicToken(ctx, t, c),
				Updates:           []types.RegexPatternSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateRegexPatternSet(
			ctx,
			&sdk.UpdateRegexPatternSetInput{
				RegexPatternSetId: id,
				ChangeToken:       classicToken(ctx, t, c),
				Updates:           []types.RegexPatternSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteRegexPatternSet(
			ctx,
			&sdk.DeleteRegexPatternSetInput{RegexPatternSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetRegexPatternSet(ctx, &sdk.GetRegexPatternSetInput{RegexPatternSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
	t.Run("RegexMatchSet", func(t *testing.T) {
		created, err := c.CreateRegexMatchSet(
			ctx,
			&sdk.CreateRegexMatchSetInput{Name: new("RegexMatchSet"), ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		id := created.RegexMatchSet.RegexMatchSetId
		item := types.RegexMatchTuple{
			FieldToMatch:       &types.FieldToMatch{Type: types.MatchFieldTypeUri},
			TextTransformation: types.TextTransformationNone,
			RegexPatternSetId:  patternID,
		}

		update := types.RegexMatchSetUpdate{Action: types.ChangeActionInsert, RegexMatchTuple: &item}
		_, err = c.UpdateRegexMatchSet(
			ctx,
			&sdk.UpdateRegexMatchSetInput{
				RegexMatchSetId: id,
				ChangeToken:     classicToken(ctx, t, c),
				Updates:         []types.RegexMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.GetRegexMatchSet(ctx, &sdk.GetRegexMatchSetInput{RegexMatchSetId: id})
		if err != nil || len(got.RegexMatchSet.RegexMatchTuples) != 1 {
			t.Fatalf("get: %v %v", got, err)
		}

		listed, err := c.ListRegexMatchSets(ctx, &sdk.ListRegexMatchSetsInput{Limit: 100})
		if err != nil || len(listed.RegexMatchSets) < 1 {
			t.Fatalf("list: %v %v", listed, err)
		}

		_, err = c.DeleteRegexMatchSet(
			ctx,
			&sdk.DeleteRegexMatchSetInput{RegexMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFNonEmptyEntityException")
		_, err = c.UpdateRegexMatchSet(
			ctx,
			&sdk.UpdateRegexMatchSetInput{
				RegexMatchSetId: id,
				ChangeToken:     classicToken(ctx, t, c),
				Updates:         []types.RegexMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFInvalidOperationException")
		update.Action = types.ChangeActionDelete
		_, err = c.UpdateRegexMatchSet(
			ctx,
			&sdk.UpdateRegexMatchSetInput{
				RegexMatchSetId: id,
				ChangeToken:     classicToken(ctx, t, c),
				Updates:         []types.RegexMatchSetUpdate{update},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UpdateRegexMatchSet(
			ctx,
			&sdk.UpdateRegexMatchSetInput{
				RegexMatchSetId: id,
				ChangeToken:     classicToken(ctx, t, c),
				Updates:         []types.RegexMatchSetUpdate{update},
			},
		)
		classicError(t, err, "WAFNonexistentItemException")
		_, err = c.DeleteRegexMatchSet(
			ctx,
			&sdk.DeleteRegexMatchSetInput{RegexMatchSetId: id, ChangeToken: classicToken(ctx, t, c)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetRegexMatchSet(ctx, &sdk.GetRegexMatchSetInput{RegexMatchSetId: id})
		classicError(t, err, "WAFNonexistentItemException")
	})
}

func TestWAFClassicTokensRulesACLTagsAndIsolation(t *testing.T) {
	s := server(t)
	c := wafClassicClient(s, "test", "us-east-1")
	ctx := context.Background()
	token := classicToken(ctx, t, c)
	status, err := c.GetChangeTokenStatus(ctx, &sdk.GetChangeTokenStatusInput{ChangeToken: token})
	if err != nil || status.ChangeTokenStatus != types.ChangeTokenStatusProvisioned {
		t.Fatalf("provisioned: %v %v", status, err)
	}

	ip, err := c.CreateIPSet(ctx, &sdk.CreateIPSetInput{Name: new("clients"), ChangeToken: token})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []types.ChangeTokenStatus{types.ChangeTokenStatusPending, types.ChangeTokenStatusInsync} {
		status, err = c.GetChangeTokenStatus(ctx, &sdk.GetChangeTokenStatusInput{ChangeToken: token})
		if err != nil || status.ChangeTokenStatus != want {
			t.Fatalf("status: %v %v", status, err)
		}
	}

	_, err = c.CreateIPSet(ctx, &sdk.CreateIPSetInput{Name: new("stale"), ChangeToken: token})
	classicError(t, err, "WAFStaleDataException")
	_, err = c.CreateIPSet(ctx, &sdk.CreateIPSetInput{Name: new("unknown"), ChangeToken: new("unknown-token")})
	classicError(t, err, "WAFStaleDataException")
	// Failed changes leave tokens usable.
	token = classicToken(ctx, t, c)
	_, err = c.CreateRule(
		ctx,
		&sdk.CreateRuleInput{Name: new("bad"), MetricName: new("invalid metric"), ChangeToken: token},
	)
	classicError(t, err, "WAFInvalidParameterException")
	rule, err := c.CreateRule(ctx, &sdk.CreateRuleInput{
		Name: new("rule"), MetricName: new("RuleMetric"), ChangeToken: token,
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	pred := types.Predicate{Type: types.PredicateTypeIpMatch, DataId: ip.IPSet.IPSetId, Negated: new(false)}
	ru := types.RuleUpdate{Action: types.ChangeActionInsert, Predicate: &pred}
	_, err = c.UpdateRule(
		ctx,
		&sdk.UpdateRuleInput{
			RuleId:      rule.Rule.RuleId,
			ChangeToken: classicToken(ctx, t, c),
			Updates:     []types.RuleUpdate{ru},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteIPSet(ctx, &sdk.DeleteIPSetInput{IPSetId: ip.IPSet.IPSetId, ChangeToken: classicToken(ctx, t, c)})
	classicError(t, err, "WAFReferencedItemException")
	gotRule, err := c.GetRule(ctx, &sdk.GetRuleInput{RuleId: rule.Rule.RuleId})
	if err != nil || len(gotRule.Rule.Predicates) != 1 {
		t.Fatalf("rule: %v %v", gotRule, err)
	}

	listedRules, err := c.ListRules(ctx, &sdk.ListRulesInput{})
	if err != nil || len(listedRules.Rules) != 1 {
		t.Fatalf("rules: %v %v", listedRules, err)
	}

	rate, err := c.CreateRateBasedRule(
		ctx,
		&sdk.CreateRateBasedRuleInput{
			Name:        new("rate"),
			MetricName:  new("RateMetric"),
			RateKey:     types.RateKeyIp,
			RateLimit:   new(int64(100)),
			ChangeToken: classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateRateBasedRule(
		ctx,
		&sdk.UpdateRateBasedRuleInput{
			RuleId:      rate.Rule.RuleId,
			RateLimit:   new(int64(200)),
			Updates:     []types.RuleUpdate{ru},
			ChangeToken: classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gotRate, err := c.GetRateBasedRule(ctx, &sdk.GetRateBasedRuleInput{RuleId: rate.Rule.RuleId})
	if err != nil || *gotRate.Rule.RateLimit != 200 || len(gotRate.Rule.MatchPredicates) != 1 {
		t.Fatalf("rate: %v %v", gotRate, err)
	}

	rates, err := c.ListRateBasedRules(ctx, &sdk.ListRateBasedRulesInput{})
	if err != nil || len(rates.Rules) != 1 {
		t.Fatalf("rates: %v %v", rates, err)
	}

	keys, err := c.GetRateBasedRuleManagedKeys(ctx, &sdk.GetRateBasedRuleManagedKeysInput{RuleId: rate.Rule.RuleId})
	if err != nil || len(keys.ManagedKeys) != 0 {
		t.Fatalf("keys: %v %v", keys, err)
	}

	acl, err := c.CreateWebACL(
		ctx,
		&sdk.CreateWebACLInput{
			Name:          new("acl"),
			MetricName:    new("ACLMetric"),
			DefaultAction: &types.WafAction{Type: types.WafActionTypeAllow},
			ChangeToken:   classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	activated := types.ActivatedRule{
		RuleId:   rule.Rule.RuleId,
		Priority: new(int32(1)),
		Type:     types.WafRuleTypeRegular,
		Action:   &types.WafAction{Type: types.WafActionTypeBlock},
	}

	activatedRate := types.ActivatedRule{
		RuleId:   rate.Rule.RuleId,
		Priority: new(int32(2)),
		Type:     types.WafRuleTypeRateBased,
		Action:   &types.WafAction{Type: types.WafActionTypeCount},
	}

	updates := []types.WebACLUpdate{
		{Action: types.ChangeActionInsert, ActivatedRule: &activated},
		{Action: types.ChangeActionInsert, ActivatedRule: &activatedRate},
	}

	_, err = c.UpdateWebACL(
		ctx,
		&sdk.UpdateWebACLInput{WebACLId: acl.WebACL.WebACLId, Updates: updates, ChangeToken: classicToken(ctx, t, c)},
	)
	if err != nil {
		t.Fatal(err)
	}

	gotACL, err := c.GetWebACL(ctx, &sdk.GetWebACLInput{WebACLId: acl.WebACL.WebACLId})
	if err != nil || len(gotACL.WebACL.Rules) != 2 {
		t.Fatalf("acl: %v %v", gotACL, err)
	}

	acls, err := c.ListWebACLs(ctx, &sdk.ListWebACLsInput{})
	if err != nil || len(acls.WebACLs) != 1 {
		t.Fatalf("acls: %v %v", acls, err)
	}

	for _, foreign := range []*sdk.Client{wafClassicClient(s, "team-b", "us-east-1")} {
		_, err = foreign.GetIPSet(ctx, &sdk.GetIPSetInput{IPSetId: ip.IPSet.IPSetId})
		classicError(t, err, "WAFNonexistentItemException")
		_, err = foreign.CreateIPSet(
			ctx,
			&sdk.CreateIPSetInput{Name: new("foreign"), ChangeToken: classicToken(ctx, t, c)},
		)
		classicError(t, err, "WAFStaleDataException")
	}

	_, err = wafClassicClient(s, "test", "eu-west-1").GetWebACL(ctx, &sdk.GetWebACLInput{WebACLId: acl.WebACL.WebACLId})
	if err != nil {
		t.Fatal("global records must span regions", err)
	}

	arn := acl.WebACL.WebACLArn
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceARN: arn, Tags: []types.Tag{{Key: new("team"), Value: new("platform")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: arn})
	if err != nil || len(tags.TagInfoForResource.TagList) != 1 {
		t.Fatalf("tags: %v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	// Removing predicates empties the rule, but its ACL reference still prevents deletion.
	ru.Action = types.ChangeActionDelete
	_, err = c.UpdateRule(
		ctx,
		&sdk.UpdateRuleInput{
			RuleId:      rule.Rule.RuleId,
			Updates:     []types.RuleUpdate{ru},
			ChangeToken: classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteRule(ctx, &sdk.DeleteRuleInput{RuleId: rule.Rule.RuleId, ChangeToken: classicToken(ctx, t, c)})
	classicError(t, err, "WAFReferencedItemException")
	for i := range updates {
		updates[i].Action = types.ChangeActionDelete
	}

	_, err = c.UpdateWebACL(
		ctx,
		&sdk.UpdateWebACLInput{
			WebACLId:      acl.WebACL.WebACLId,
			Updates:       updates,
			DefaultAction: &types.WafAction{Type: types.WafActionTypeBlock},
			ChangeToken:   classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteWebACL(
		ctx,
		&sdk.DeleteWebACLInput{WebACLId: acl.WebACL.WebACLId, ChangeToken: classicToken(ctx, t, c)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteRule(ctx, &sdk.DeleteRuleInput{RuleId: rule.Rule.RuleId, ChangeToken: classicToken(ctx, t, c)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateRateBasedRule(
		ctx,
		&sdk.UpdateRateBasedRuleInput{
			RuleId:      rate.Rule.RuleId,
			RateLimit:   new(int64(200)),
			Updates:     []types.RuleUpdate{ru},
			ChangeToken: classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteRateBasedRule(
		ctx,
		&sdk.DeleteRateBasedRuleInput{RuleId: rate.Rule.RuleId, ChangeToken: classicToken(ctx, t, c)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteIPSet(ctx, &sdk.DeleteIPSetInput{IPSetId: ip.IPSet.IPSetId, ChangeToken: classicToken(ctx, t, c)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWAFClassicAtomicTokensPaginationAndRegexReferences(t *testing.T) {
	c := wafClassicClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	token := classicToken(ctx, t, c)
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func() {
			_, err := c.CreateIPSet(ctx, &sdk.CreateIPSetInput{Name: new(name), ChangeToken: token})
			results <- err
		}()
	}

	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else {
			classicError(t, err, "WAFStaleDataException")
		}
	}

	if successes != 1 {
		t.Fatalf("single-use token had %d successful mutations", successes)
	}

	for _, name := range []string{"three", "four"} {
		if _, err := c.CreateIPSet(
			ctx,
			&sdk.CreateIPSetInput{Name: new(name), ChangeToken: classicToken(ctx, t, c)},
		); err != nil {
			t.Fatal(err)
		}
	}

	page, err := c.ListIPSets(ctx, &sdk.ListIPSetsInput{Limit: 1})
	if err != nil || len(page.IPSets) != 1 || page.NextMarker == nil {
		t.Fatalf("page: %v %v", page, err)
	}

	second, err := c.ListIPSets(ctx, &sdk.ListIPSetsInput{Limit: 1, NextMarker: page.NextMarker})
	if err != nil || len(second.IPSets) != 1 || *second.IPSets[0].IPSetId == *page.IPSets[0].IPSetId {
		t.Fatalf("second page: %v %v", second, err)
	}

	token = classicToken(ctx, t, c)
	_, err = c.UpdateIPSet(
		ctx,
		&sdk.UpdateIPSetInput{
			IPSetId:     page.IPSets[0].IPSetId,
			ChangeToken: token,
			Updates: []types.IPSetUpdate{
				{
					Action: types.ChangeActionInsert,
					IPSetDescriptor: &types.IPSetDescriptor{
						Type:  types.IPSetDescriptorTypeIpv4,
						Value: new("not-a-cidr"),
					},
				},
			},
		},
	)
	classicError(t, err, "WAFInvalidParameterException")
	status, err := c.GetChangeTokenStatus(ctx, &sdk.GetChangeTokenStatusInput{ChangeToken: token})
	if err != nil || status.ChangeTokenStatus != types.ChangeTokenStatusProvisioned {
		t.Fatalf("failed change consumed token: %v %v", status, err)
	}

	pattern, err := c.CreateRegexPatternSet(
		ctx,
		&sdk.CreateRegexPatternSetInput{Name: new("pattern"), ChangeToken: token},
	)
	if err != nil {
		t.Fatal(err)
	}

	match, err := c.CreateRegexMatchSet(
		ctx,
		&sdk.CreateRegexMatchSetInput{Name: new("match"), ChangeToken: classicToken(ctx, t, c)},
	)
	if err != nil {
		t.Fatal(err)
	}

	tuple := &types.RegexMatchTuple{
		FieldToMatch:       &types.FieldToMatch{Type: types.MatchFieldTypeUri},
		TextTransformation: types.TextTransformationNone,
		RegexPatternSetId:  pattern.RegexPatternSet.RegexPatternSetId,
	}

	_, err = c.UpdateRegexMatchSet(
		ctx,
		&sdk.UpdateRegexMatchSetInput{
			RegexMatchSetId: match.RegexMatchSet.RegexMatchSetId,
			ChangeToken:     classicToken(ctx, t, c),
			Updates:         []types.RegexMatchSetUpdate{{Action: types.ChangeActionInsert, RegexMatchTuple: tuple}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteRegexPatternSet(
		ctx,
		&sdk.DeleteRegexPatternSetInput{
			RegexPatternSetId: pattern.RegexPatternSet.RegexPatternSetId,
			ChangeToken:       classicToken(ctx, t, c),
		},
	)
	classicError(t, err, "WAFReferencedItemException")
}

func TestWAFClassicLogging(t *testing.T) {
	s := server(t)
	c := wafClassicClient(s, "test", "us-east-1")
	ctx := context.Background()
	acl, err := c.CreateWebACL(
		ctx,
		&sdk.CreateWebACLInput{
			Name:          new("logged"),
			MetricName:    new("LoggedACL"),
			DefaultAction: &types.WafAction{Type: types.WafActionTypeAllow},
			ChangeToken:   classicToken(ctx, t, c),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	arn := acl.WebACL.WebACLArn
	_, err = c.GetLoggingConfiguration(ctx, &sdk.GetLoggingConfigurationInput{ResourceArn: arn})
	classicError(t, err, "WAFNonexistentItemException")
	config := &types.LoggingConfiguration{
		ResourceArn:           arn,
		LogDestinationConfigs: []string{"arn:aws:firehose:us-east-1:000000000000:deliverystream/aws-waf-logs-classic"},
		RedactedFields:        []types.FieldToMatch{{Type: types.MatchFieldTypeHeader, Data: new("authorization")}},
	}

	_, err = c.PutLoggingConfiguration(ctx, &sdk.PutLoggingConfigurationInput{LoggingConfiguration: config})
	classicError(t, err, "WAFInvalidParameterException")
	_, err = firehoseClient(s).CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
		DeliveryStreamName: new("aws-waf-logs-classic"),
		ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
			BucketARN: new("arn:aws:s3:::logs"), RoleARN: new("arn:aws:iam::000000000000:role/firehose"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutLoggingConfiguration(ctx, &sdk.PutLoggingConfigurationInput{LoggingConfiguration: config})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetLoggingConfiguration(ctx, &sdk.GetLoggingConfigurationInput{ResourceArn: arn})
	if err != nil || len(got.LoggingConfiguration.LogDestinationConfigs) != 1 ||
		len(got.LoggingConfiguration.RedactedFields) != 1 {
		t.Fatalf("logging: %v %v", got, err)
	}

	list, err := c.ListLoggingConfigurations(ctx, &sdk.ListLoggingConfigurationsInput{Limit: 1})
	if err != nil || len(list.LoggingConfigurations) != 1 {
		t.Fatalf("logging list: %v %v", list, err)
	}

	_, err = wafClassicClient(
		s,
		"team-b",
		"us-east-1",
	).GetLoggingConfiguration(ctx, &sdk.GetLoggingConfigurationInput{ResourceArn: arn})
	classicError(t, err, "WAFNonexistentItemException")
	_, err = c.DeleteLoggingConfiguration(ctx, &sdk.DeleteLoggingConfigurationInput{ResourceArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetLoggingConfiguration(ctx, &sdk.GetLoggingConfigurationInput{ResourceArn: arn})
	classicError(t, err, "WAFNonexistentItemException")
}
