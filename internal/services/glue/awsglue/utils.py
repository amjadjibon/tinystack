"""Glue-style argument resolution for Python shell scripts."""

import argparse


class GlueArgumentError(Exception):
    """An invalid or missing job argument."""


class GlueArgumentParser(argparse.ArgumentParser):
    def error(self, message):
        raise GlueArgumentError(message)


_CONTINUATIONS = {
    "continuation-enabled": "job-bookmark-enable",
    "continuation-readonly": "job-bookmark-pause",
    "continuation-ignore": "job-bookmark-disable",
}
_RESERVED = {
    "JOB_ID": None,
    "JOB_RUN_ID": None,
    "SECURITY_CONFIGURATION": None,
    "continuation-option": tuple(_CONTINUATIONS),
    "job-bookmark-option": tuple(_CONTINUATIONS.values()),
    "job-bookmark-from": None,
    "job-bookmark-to": None,
    "encryption-type": ("sse-s3",),
    "enable-data-lineage": None,
}


def getResolvedOptions(args, options, allow_abbrev=False):
    """Resolve requested arguments and the Glue runtime's reserved options."""
    parser = GlueArgumentParser(allow_abbrev=allow_abbrev, add_help=False)
    requested = set(options)
    for option, choices in _RESERVED.items():
        if option != "JOB_RUN_ID" and (
            option in requested or option.replace("-", "_") in requested
        ):
            raise RuntimeError("Using reserved arguments --" + option)
        parser.add_argument(
            "--" + option, choices=choices, required=option == "JOB_RUN_ID" and option in requested
        )

    temp_options = ("TempDir", "RedshiftTempDir")
    for option in temp_options:
        parser.add_argument("--" + option)
    for option in options:
        if not option or option.startswith("-"):
            raise GlueArgumentError("Option names must omit the leading '--'")
        if option != "JOB_RUN_ID" and option not in temp_options:
            parser.add_argument("--" + option, required=True)

    parsed, _ = parser.parse_known_args(args[1:])
    result = vars(parsed)
    temp_dir = result["TempDir"]
    if temp_dir is None:
        temp_dir = result["RedshiftTempDir"]
    result["TempDir"] = result["RedshiftTempDir"] = temp_dir

    continuation = result.pop("continuation_option")
    mode = result["job_bookmark_option"]
    if mode is None:
        mode = _CONTINUATIONS.get(continuation, "job-bookmark-disable")
        result["job_bookmark_option"] = mode
    has_from = result["job_bookmark_from"] is not None
    has_to = result["job_bookmark_to"] is not None
    if mode == "job-bookmark-pause" and has_from != has_to:
        raise RuntimeError("job-bookmark-pause requires both range arguments when either is supplied")
    if mode != "job-bookmark-pause" and (has_from or has_to):
        raise RuntimeError("Bookmark range arguments require job-bookmark-pause")
    return result
