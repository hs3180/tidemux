"""Assertions shared by package gates for the persisted runtime envelope."""
import re


def validate_runtime_events(events, version, one_instance=True):
    identities = set()
    instances = set()
    for event in events:
        service = event.get("service", {})
        instance = service.get("instance", {}).get("id", "")
        identity = event.get("event_id", "")
        if service.get("name") != "tidemux" or service.get("version") != version:
            raise RuntimeError("runtime service metadata disagrees with binary version")
        if not re.fullmatch(r"[0-9a-f]{32}", instance) or not re.fullmatch(instance + r"-[0-9a-f]{16}", identity):
            raise RuntimeError("invalid runtime identity")
        if identity in identities:
            raise RuntimeError("distinct events reused an event ID")
        identities.add(identity)
        instances.add(instance)
    if not events or (one_instance and len(instances) != 1):
        raise RuntimeError("events do not share one process instance")
    return instances
