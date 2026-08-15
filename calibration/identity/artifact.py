"""Strict JSON and canonical hashing shared by aggregate-only calibration tools."""

import hashlib
import json


def load_strict_json(content):
    """Decode one integer-only JSON value without duplicates or nulls."""
    if not isinstance(content, bytes):
        raise ValueError("strict JSON content must be bytes")

    def object_without_duplicates(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate JSON field {key!r}")
            result[key] = value
        return result

    def unsigned_integer(value):
        if not value.isdigit() or (len(value) > 1 and value[0] == "0"):
            raise ValueError("JSON numbers must be unsigned decimal integers")
        return int(value)

    def reject_float(_value):
        raise ValueError("JSON floating-point values are not allowed")

    def reject_constant(_value):
        raise ValueError("non-standard JSON numbers are not allowed")

    try:
        document = json.loads(
            content.decode("utf-8", errors="strict"),
            object_pairs_hook=object_without_duplicates,
            parse_int=unsigned_integer,
            parse_float=reject_float,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise ValueError("invalid strict JSON document") from error
    _reject_null(document)
    if not isinstance(document, dict):
        raise ValueError("strict JSON root must be an object")
    return document


def canonical_json_hash(document):
    """Hash one validated integer-only document with sorted JSON object keys."""
    _validate_canonical_value(document)
    encoded = json.dumps(
        document,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("ascii")
    return hashlib.sha256(encoded).hexdigest()


def _reject_null(value):
    if value is None:
        raise ValueError("null is not allowed")
    if isinstance(value, dict):
        for item in value.values():
            _reject_null(item)
    elif isinstance(value, list):
        for item in value:
            _reject_null(item)


def _validate_canonical_value(value):
    if value is None or isinstance(value, float):
        raise ValueError("canonical JSON accepts neither null nor floating point")
    if isinstance(value, bool) or isinstance(value, str):
        return
    if isinstance(value, int):
        if value < 0:
            raise ValueError("canonical JSON integers must be unsigned")
        return
    if isinstance(value, list):
        for item in value:
            _validate_canonical_value(item)
        return
    if isinstance(value, dict):
        for key, item in value.items():
            if not isinstance(key, str):
                raise ValueError("canonical JSON object keys must be strings")
            _validate_canonical_value(item)
        return
    raise ValueError("canonical JSON contains an unsupported value")
