package emr

const (
	master            = "MASTER"
	core              = "CORE"
	task              = "TASK"
	running           = "RUNNING"
	pending           = "PENDING"
	completed         = "COMPLETED"
	cancelled         = "CANCELLED"
	continueSteps     = "CONTINUE"
	terminated        = "TERMINATED"
	terminating       = "TERMINATING"
	waiting           = "WAITING"
	cancelAndWait     = "CANCEL_AND_WAIT"
	terminateCluster  = "TERMINATE_CLUSTER"
	onDemand          = "ON_DEMAND"
	instanceGroups    = "INSTANCE_GROUP"
	instanceFleets    = "INSTANCE_FLEET"
	userRequest       = "USER_REQUEST"
	allStepsCompleted = "ALL_STEPS_COMPLETED"
)
