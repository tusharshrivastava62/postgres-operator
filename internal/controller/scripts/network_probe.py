#!/usr/bin/env python3
"""Tries once to open a connection to the cluster's database port, from a pod
that a network policy should refuse, and reports what happened.

Run on a schedule by the operator, in a pod that carries none of the labels
the cluster's network policy admits. A policy does nothing on a network plugin
that ignores it, so the only way to know one is enforced is to be a pod it
should keep out, and try. The result is one word in the termination log:
  open         the connection was made, so nothing stopped it
  timeout      no answer, which is how a policy that drops packets looks
  refused      the connection was turned away
  unreachable  the address could not be reached at all
  error        anything else, such as a name that did not resolve
The operator decides what it means, since a database that is down looks the
same as one that is protected.
"""

import json
import os
import socket
import sys

TERMINATION_LOG = os.environ.get("TERMINATION_LOG", "/dev/termination-log")
HOST = os.environ.get("TARGET_HOST", "")
PORT = int(os.environ.get("TARGET_PORT", "5432"))
TIMEOUT = float(os.environ.get("CONNECT_TIMEOUT", "5"))


def probe():
    if not HOST:
        return {"result": "error", "detail": "TARGET_HOST is not set"}
    try:
        with socket.create_connection((HOST, PORT), timeout=TIMEOUT):
            return {"result": "open"}
    except socket.timeout:
        return {"result": "timeout"}
    except ConnectionRefusedError:
        return {"result": "refused"}
    except socket.gaierror as exc:
        return {"result": "error", "detail": "could not resolve %s: %s" % (HOST, exc)}
    except OSError as exc:
        # Unreachable network or host, as opposed to one that did not answer.
        if getattr(exc, "errno", None) in (101, 113):
            return {"result": "unreachable", "detail": str(exc)}
        return {"result": "error", "detail": str(exc)[:120]}


def main():
    with open(TERMINATION_LOG, "w") as handle:
        json.dump(probe(), handle, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
