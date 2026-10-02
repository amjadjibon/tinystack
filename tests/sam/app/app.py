import json
import os


def handler(event, context):
    return {
        "statusCode": 200,
        "headers": {"Content-Type": "application/json"},
        "body": json.dumps({"greeting": os.environ["GREETING"], "release": "one", "path": event.get("rawPath") or event.get("path")}),
    }
