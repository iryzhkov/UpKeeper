"""Stable public constants for the fleet CLI."""

SCHEMA_VERSION = 1

EXIT_SUCCESS = 0
EXIT_DRIFT = 10
EXIT_PARTIAL = 20
EXIT_PREFLIGHT = 30
EXIT_DEPLOYMENT = 40
EXIT_ROLLBACK = 50
EXIT_LOCK_CONTENTION = 60

EXIT_NAMES = {
    EXIT_SUCCESS: "success",
    EXIT_DRIFT: "drift",
    EXIT_PARTIAL: "partial",
    EXIT_PREFLIGHT: "preflight-failure",
    EXIT_DEPLOYMENT: "deployment-failure",
    EXIT_ROLLBACK: "rollback-failure",
    EXIT_LOCK_CONTENTION: "lock-contention",
}
