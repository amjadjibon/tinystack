package budgets

import (
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	timeAnnually  = "ANNUALLY"
	timeMonthly   = "MONTHLY"
	timeQuarterly = "QUARTERLY"
	budgetCost    = "COST"
	budgetUsage   = "USAGE"
)

var (
	accountPattern = regexp.MustCompile(`^\d{12}$`)
	amountPattern  = regexp.MustCompile(`^(\d*\.)?\d+$`)
)

func checkName(name *string) error {
	n := common.Value(name)
	lower := strings.ToLower(n)
	if utf8.RuneCountInString(n) < 1 || utf8.RuneCountInString(n) > 100 || strings.ContainsAny(n, ":\\") ||
		strings.Contains(n, "/action/") || strings.Contains(lower, "<script>") && strings.Contains(lower, "</script>") {
		return invalid("BudgetName violates the modeled length or pattern.")
	}

	return nil
}

func checkSpend(s api.Spend) error {
	if s.Amount == nil || !amountPattern.MatchString(*s.Amount) || common.Value(s.Unit) == "" {
		return invalid("Spend requires a nonnegative decimal Amount and a nonempty Unit.")
	}

	return nil
}

func checkBudget(b *api.Budget) error {
	if b != nil && b.CalculatedSpend != nil {
		return invalid("CalculatedSpend is read-only.")
	}

	if b == nil {
		return invalid("Budget is required.")
	}

	if err := checkName(b.BudgetName); err != nil {
		return err
	}

	if !slices.Contains(
		[]string{"DAILY", timeMonthly, timeQuarterly, timeAnnually, "CUSTOM"},
		common.Value(b.TimeUnit),
	) {
		return invalid("TimeUnit is invalid.")
	}

	if !slices.Contains(
		[]string{
			budgetCost,
			budgetUsage,
			"RI_UTILIZATION",
			"RI_COVERAGE",
			"SAVINGS_PLANS_UTILIZATION",
			"SAVINGS_PLANS_COVERAGE",
		},
		common.Value(b.BudgetType),
	) {
		return invalid("BudgetType is invalid.")
	}

	if b.BudgetLimit != nil && len(b.PlannedBudgetLimits) > 0 {
		return invalid("BudgetLimit and PlannedBudgetLimits are mutually exclusive.")
	}

	if b.BudgetLimit != nil {
		if err := checkSpend(*b.BudgetLimit); err != nil {
			return err
		}
	}

	if len(b.PlannedBudgetLimits) > 0 && (!slices.Contains([]string{budgetCost, budgetUsage}, *b.BudgetType) ||
		!slices.Contains([]string{timeMonthly, timeQuarterly}, *b.TimeUnit)) {
		return invalid("PlannedBudgetLimits requires a monthly or quarterly COST or USAGE budget.")
	}

	for period := range b.PlannedBudgetLimits {
		epoch, err := strconv.ParseInt(period, 10, 64)
		if err != nil || epoch < 0 {
			return invalid("PlannedBudgetLimits keys must be UTC epoch seconds.")
		}
	}

	if *b.BudgetType != budgetCost && *b.BudgetType != budgetUsage && b.BudgetLimit != nil {
		amount, ok := new(big.Rat).SetString(*b.BudgetLimit.Amount)
		if !ok || amount.Cmp(big.NewRat(100, 1)) != 0 {
			return invalid("Utilization and coverage budgets require a limit of 100.")
		}
	}

	if b.CostTypes != nil && *b.BudgetType != budgetCost {
		return invalid("CostTypes is only valid for COST budgets.")
	}

	for _, spend := range b.PlannedBudgetLimits {
		if err := checkSpend(spend); err != nil {
			return err
		}
	}

	if b.BudgetLimit == nil && len(b.PlannedBudgetLimits) == 0 {
		if common.Value(b.BudgetType) == budgetCost || common.Value(b.BudgetType) == budgetUsage {
			return invalid("A budget limit is required.")
		}

		b.BudgetLimit = &api.Spend{Amount: new("100"), Unit: new("%")}
	}

	if b.TimePeriod == nil {
		b.TimePeriod = &api.TimePeriod{}
	}

	if b.TimePeriod.Start == nil {
		now := time.Now().UTC()
		year, month, day := now.Date()
		switch common.Value(b.TimeUnit) {
		case timeMonthly:
			day = 1
		case timeQuarterly:
			day = 1
			month = ((month-1)/3)*3 + 1
		case timeAnnually:
			day = 1
			month = time.January
		case "CUSTOM":
			return invalid("CUSTOM budgets require a start date.")
		}

		b.TimePeriod.Start = new(float64(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix()))
	}

	end := float64(time.Date(2087, time.June, 15, 0, 0, 0, 0, time.UTC).Unix())
	if b.TimePeriod.End == nil {
		b.TimePeriod.End = new(end)
	}

	if math.IsNaN(*b.TimePeriod.Start) || math.IsInf(*b.TimePeriod.Start, 0) ||
		math.IsNaN(*b.TimePeriod.End) || *b.TimePeriod.Start >= *b.TimePeriod.End || *b.TimePeriod.End > end {
		return invalid("TimePeriod must have a start before its end, no later than June 15, 2087.")
	}

	return nil
}

func checkNotification(n *api.Notification) error {
	if n == nil {
		return invalid("Notification is required.")
	}

	if !slices.Contains([]string{"ACTUAL", "FORECASTED"}, common.Value(n.NotificationType)) ||
		!slices.Contains([]string{"GREATER_THAN", "LESS_THAN", "EQUAL_TO"}, common.Value(n.ComparisonOperator)) {
		return invalid("NotificationType or ComparisonOperator is invalid.")
	}

	if n.Threshold == nil || math.IsNaN(*n.Threshold) || *n.Threshold < 0 || *n.Threshold > 15000000000000 {
		return invalid("Threshold must be between 0 and 15000000000000.")
	}

	if n.ThresholdType == nil {
		n.ThresholdType = new("PERCENTAGE")
	}

	if !slices.Contains([]string{"PERCENTAGE", "ABSOLUTE_VALUE"}, *n.ThresholdType) {
		return invalid("ThresholdType is invalid.")
	}

	if n.NotificationState != nil && !slices.Contains([]string{"OK", "ALARM"}, *n.NotificationState) {
		return invalid("NotificationState is invalid.")
	}

	return nil
}

func sameNotification(a, b *api.Notification) bool {
	sameType := common.Value(a.NotificationType) == common.Value(b.NotificationType)
	sameOperator := common.Value(a.ComparisonOperator) == common.Value(b.ComparisonOperator)
	sameThreshold := common.Value(a.Threshold) == common.Value(b.Threshold)
	sameThresholdType := common.Value(a.ThresholdType) == common.Value(b.ThresholdType)
	return sameType && sameOperator && sameThreshold && sameThresholdType
}

func notificationIndex(r *record, n *api.Notification) (int, error) {
	if err := checkNotification(n); err != nil {
		return 0, err
	}

	for i, v := range r.Notifications {
		if sameNotification(v.Notification, n) {
			return i, nil
		}
	}

	return 0, missing()
}

func checkSubscriber(s *api.Subscriber) error {
	if s == nil || !slices.Contains([]string{"SNS", "EMAIL"}, common.Value(s.SubscriptionType)) ||
		common.Value(s.Address) == "" || strings.ContainsAny(common.Value(s.Address), "\r\n") {
		return invalid("Subscriber requires SNS or EMAIL and an address without line breaks.")
	}

	return nil
}

func sameSubscriber(a, b api.Subscriber) bool {
	return common.Value(a.SubscriptionType) == common.Value(b.SubscriptionType) &&
		common.Value(a.Address) == common.Value(b.Address)
}

func subscriberIndex(subs []api.Subscriber, s *api.Subscriber) (int, error) {
	if err := checkSubscriber(s); err != nil {
		return 0, err
	}

	for i, v := range subs {
		if sameSubscriber(v, *s) {
			return i, nil
		}
	}

	return 0, missing()
}

// AWS CreateBudget/CreateNotification document five notifications and ten EMAIL plus one SNS subscriber.
// https://docs.aws.amazon.com/cli/latest/reference/budgets/create-budget.html
func checkSubscribers(subs []api.Subscriber) error {
	if len(subs) == 0 {
		return invalid("At least one subscriber is required.")
	}

	email, sns := 0, 0
	for i, v := range subs {
		if err := checkSubscriber(&v); err != nil {
			return err
		}

		for _, old := range subs[:i] {
			if sameSubscriber(old, v) {
				return duplicate()
			}
		}

		if *v.SubscriptionType == "SNS" {
			sns++
		} else {
			email++
		}
	}

	if email > 10 || sns > 1 {
		return awserror.New(
			"CreationLimitExceededException",
			"A notification supports ten EMAIL and one SNS subscriber.",
		)
	}

	return nil
}
