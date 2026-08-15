"""Shared leased provider session for local media workers."""

from dataclasses import dataclass
from collections.abc import Mapping
import datetime
import threading
import uuid

import grpc
from google.protobuf import duration_pb2, timestamp_pb2
from proactive.platform.v1 import adapter_pb2, capability_pb2, observation_pb2


UTC = datetime.timezone.utc
_PROVIDER_PREFIX = "PROACTIVE_PROVIDER_"


@dataclass(frozen=True)
class ProviderBinding:
    provider_id: str
    capability_name: str
    capability: int


_STACK_CAPABILITIES = {
    "vision": (
        4,
        "PERSON_PRESENCE",
        {
            "PERSON_PRESENCE": capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
            "FACE_DETECTION": capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION,
            "FACE_IDENTIFICATION": capability_pb2.SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION,
            "FACE_LIVENESS": capability_pb2.SERVICE_CAPABILITY_KIND_FACE_LIVENESS,
        },
    ),
    "audio": (
        3,
        "VOICE_ACTIVITY",
        {
            "VOICE_ACTIVITY": capability_pb2.SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY,
            "SPEAKER_IDENTIFICATION": capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION,
            "SPEAKER_VERIFICATION": capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION,
        },
    ),
}


def parse_provider_bindings(environment, stack):
    """Parse the exact Supervisor-owned provider subset for one device stack."""

    if not isinstance(environment, Mapping) or stack not in _STACK_CAPABILITIES:
        raise ValueError("provider environment and known stack are required")
    if any(not isinstance(key, str) or not isinstance(value, str) for key, value in environment.items()):
        raise ValueError("provider environment keys and values must be strings")
    maximum, base, allowed = _STACK_CAPABILITIES[stack]
    count_value = environment.get("PROACTIVE_PROVIDER_COUNT")
    try:
        count = int(count_value)
    except (TypeError, ValueError) as error:
        raise ValueError("provider count is required") from error
    if str(count) != count_value or not 1 <= count <= maximum:
        raise ValueError("provider count is invalid for stack")

    expected = {"PROACTIVE_PROVIDER_COUNT"}
    for index in range(count):
        expected.add(f"PROACTIVE_PROVIDER_{index}_ID")
        expected.add(f"PROACTIVE_PROVIDER_{index}_CAPABILITY")
    actual = {key for key in environment if key.startswith(_PROVIDER_PREFIX)}
    if actual != expected:
        raise ValueError("provider environment has missing or unexpected fields")

    bindings = []
    seen_ids = set()
    seen_capabilities = set()
    for index in range(count):
        provider_id = environment[f"PROACTIVE_PROVIDER_{index}_ID"]
        capability_name = environment[f"PROACTIVE_PROVIDER_{index}_CAPABILITY"]
        if (
            not isinstance(provider_id, str)
            or not provider_id
            or provider_id.strip() != provider_id
            or len(provider_id.encode("utf-8")) > 256
            or capability_name not in allowed
            or provider_id in seen_ids
            or capability_name in seen_capabilities
        ):
            raise ValueError("provider binding is invalid")
        seen_ids.add(provider_id)
        seen_capabilities.add(capability_name)
        bindings.append(ProviderBinding(provider_id, capability_name, allowed[capability_name]))
    if base not in seen_capabilities:
        raise ValueError("provider stack base capability is required")
    if [item.provider_id for item in bindings] != sorted(item.provider_id for item in bindings):
        raise ValueError("provider bindings must be sorted by provider id")
    return tuple(bindings)


@dataclass(frozen=True)
class ProviderConfig:
    provider_id: str
    instance_id: str
    capability: int
    implementation_version: str
    privacy_class: int
    maximum_latency: datetime.timedelta
    cancellation_semantics: int
    device_requirements: tuple
    subject_id: str
    observation_ttl: datetime.timedelta
    heartbeat_interval: datetime.timedelta = datetime.timedelta(seconds=5)
    rpc_timeout: float = 2.0

    def __post_init__(self):
        required = (self.provider_id, self.instance_id, self.implementation_version, self.subject_id)
        if any(not value or value.strip() != value for value in required):
            raise ValueError("provider identifiers must be non-empty and trimmed")
        if self.capability == capability_pb2.SERVICE_CAPABILITY_KIND_UNSPECIFIED:
            raise ValueError("provider capability is required")
        if self.privacy_class not in (
            capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
            capability_pb2.PROVIDER_PRIVACY_CLASS_REMOTE_PROCESSING,
        ):
            raise ValueError("provider privacy class is required")
        if (
            not isinstance(self.maximum_latency, datetime.timedelta)
            or self.maximum_latency <= datetime.timedelta(0)
        ):
            raise ValueError("provider maximum latency must be positive")
        if self.cancellation_semantics not in (
            capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_NOT_SUPPORTED,
            capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
            capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_BOUNDED,
        ):
            raise ValueError("provider cancellation semantics are required")
        devices = tuple(self.device_requirements)
        valid_devices = {
            capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,
            capability_pb2.PROVIDER_DEVICE_CLASS_MICROPHONE,
            capability_pb2.PROVIDER_DEVICE_CLASS_DISPLAY,
            capability_pb2.PROVIDER_DEVICE_CLASS_AUDIO_OUTPUT,
            capability_pb2.PROVIDER_DEVICE_CLASS_EMBODIMENT_CONTROLLER,
        }
        duplicate_device = len(set(devices)) != len(devices)
        if any(device not in valid_devices for device in devices) or duplicate_device:
            raise ValueError("provider device requirements must be known and unique")
        object.__setattr__(self, "device_requirements", devices)
        if self.observation_ttl <= datetime.timedelta(0):
            raise ValueError("observation TTL must be positive")
        if self.heartbeat_interval <= datetime.timedelta(0) or self.rpc_timeout <= 0:
            raise ValueError("heartbeat interval and RPC timeout must be positive")


class ProviderConnection:
    """Own one process-wide channel and create independent Provider sessions."""

    def __init__(self, channel, registry, ingress):
        if channel is None or registry is None or ingress is None:
            raise ValueError("channel, registry, and ingress are required")
        self._channel = channel
        self._registry = registry
        self._ingress = ingress
        self._closed = False
        self._lock = threading.Lock()

    @classmethod
    def connect(cls, address):
        if not address or address.strip() != address:
            raise ValueError("gRPC address is required")
        channel = grpc.insecure_channel(address)
        from proactive.platform.v1 import adapter_pb2_grpc, capability_pb2_grpc

        return cls(
            channel,
            capability_pb2_grpc.CapabilityProviderRegistryServiceStub(channel),
            adapter_pb2_grpc.ObservationIngressServiceStub(channel),
        )

    @property
    def channel(self):
        return self._channel

    def session(self, config, *, utcnow=None, uuid_factory=None):
        with self._lock:
            if self._closed:
                raise RuntimeError("provider connection is closed")
        return ProviderSession(
            config,
            self._registry,
            self._ingress,
            utcnow=utcnow,
            uuid_factory=uuid_factory,
        )

    def close(self):
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._channel.close()


class ProviderSession:
    """Maintains one provider lease without retaining media or hiding a thread."""

    def __init__(self, config, registry, ingress, *, utcnow=None, uuid_factory=None, channel=None):
        if not isinstance(config, ProviderConfig) or registry is None or ingress is None:
            raise ValueError("config, registry, and ingress are required")
        self._config = config
        self._registry = registry
        self._ingress = ingress
        self._utcnow = utcnow or (lambda: datetime.datetime.now(UTC))
        self._uuid_factory = uuid_factory or uuid.uuid4
        self._channel = channel
        self._lease_id = ""
        self._expires_at = None
        self._next_heartbeat_at = None
        self._healthy = False
        self._closed = False
        self._source_seq = 0
        self._desired_health = capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY
        self._desired_reason = capability_pb2.PROVIDER_HEALTH_REASON_NONE
        self._lock = threading.RLock()

    @classmethod
    def connect(cls, address, config):
        connection = ProviderConnection.connect(address)
        session = connection.session(config)
        session._channel = connection
        return session

    @property
    def lease_id(self):
        with self._lock:
            return self._lease_id

    @property
    def capability(self):
        return self._config.capability

    @property
    def instance_id(self):
        return self._config.instance_id

    @property
    def rpc_timeout(self):
        return self._config.rpc_timeout

    @property
    def observation_ttl(self):
        return self._config.observation_ttl

    @property
    def leased(self):
        with self._lock:
            return (
                not self._closed
                and bool(self._lease_id)
                and self._expires_at is not None
                and self._now() < self._expires_at
            )

    @property
    def active(self):
        with self._lock:
            if self._closed or not self._healthy or not self._lease_id or self._expires_at is None:
                return False
            return self._now() < self._expires_at

    def start(self):
        with self._lock:
            if self._closed or not self._register():
                return False
            return self._heartbeat(self._desired_health, self._desired_reason)

    def maintain(self):
        with self._lock:
            if self._closed:
                return False
            now = self._now()
            if not self._lease_id or self._expires_at is None or now >= self._expires_at:
                self._clear_lease()
                if not self._register():
                    return False
                return self._heartbeat(self._desired_health, self._desired_reason)
            if self._next_heartbeat_at is not None and now >= self._next_heartbeat_at:
                return self._heartbeat(self._desired_health, self._desired_reason)
            return self.active

    def mark_unhealthy(self, reason):
        reasons = {
            "DEVICE_UNAVAILABLE": capability_pb2.PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE,
            "PERMISSION_DENIED": capability_pb2.PROVIDER_HEALTH_REASON_PERMISSION_DENIED,
            "DEPENDENCY_UNAVAILABLE": capability_pb2.PROVIDER_HEALTH_REASON_DEPENDENCY_UNAVAILABLE,
            "MODEL_UNAVAILABLE": capability_pb2.PROVIDER_HEALTH_REASON_MODEL_UNAVAILABLE,
            "INTERNAL_ERROR": capability_pb2.PROVIDER_HEALTH_REASON_INTERNAL_ERROR,
            "SHUTTING_DOWN": capability_pb2.PROVIDER_HEALTH_REASON_SHUTTING_DOWN,
        }
        mapped = reasons.get(reason)
        if mapped is None:
            raise ValueError("provider health reason is unknown")
        with self._lock:
            self._desired_health = capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY
            self._desired_reason = mapped
            if not self._lease_id:
                return False
            return self._heartbeat(self._desired_health, self._desired_reason)

    def mark_healthy(self):
        with self._lock:
            self._desired_health = capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY
            self._desired_reason = capability_pb2.PROVIDER_HEALTH_REASON_NONE
            if not self._lease_id:
                return False
            return self._heartbeat(self._desired_health, self._desired_reason)

    def next_source_sequence(self):
        with self._lock:
            if not self.active:
                raise RuntimeError("provider lease is not healthy")
            if self._source_seq == (1 << 64) - 1:
                raise OverflowError("provider source sequence is exhausted")
            self._source_seq += 1
            return self._source_seq

    def publish_presence(self, present):
        return self._publish(person_presence=observation_pb2.PersonPresence(present=bool(present)))

    def publish_speech_activity(self):
        return self._publish(speech_activity=observation_pb2.SpeechActivity(active=True))

    def close(self):
        with self._lock:
            if self._closed:
                return
            self._closed = True
            lease_id = self._lease_id
            self._clear_lease()
        if lease_id:
            try:
                self._registry.UnregisterCapabilityProvider(
                    capability_pb2.UnregisterCapabilityProviderRequest(lease_id=lease_id),
                    timeout=self._config.rpc_timeout,
                )
            except grpc.RpcError:
                pass
        if self._channel is not None:
            self._channel.close()

    def _register(self):
        maximum_latency = duration_pb2.Duration()
        maximum_latency.FromTimedelta(self._config.maximum_latency)
        request = capability_pb2.RegisterCapabilityProviderRequest(
            provider_id=self._config.provider_id,
            instance_id=self._config.instance_id,
            protocol_version="v1",
            implementation_version=self._config.implementation_version,
            capabilities=[self._config.capability],
            health=capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY,
            health_reason=capability_pb2.PROVIDER_HEALTH_REASON_STARTING,
            operational_profile=capability_pb2.ProviderOperationalProfile(
                privacy_class=self._config.privacy_class,
                maximum_latency=maximum_latency,
                cancellation_semantics=self._config.cancellation_semantics,
                device_requirements=self._config.device_requirements,
            ),
        )
        try:
            response = self._registry.RegisterCapabilityProvider(request, timeout=self._config.rpc_timeout)
        except grpc.RpcError:
            self._clear_lease()
            return False
        expires_at = _response_expiry(response)
        if not response.lease_id or expires_at is None or expires_at <= self._now():
            self._clear_lease()
            return False
        self._lease_id = response.lease_id
        self._expires_at = expires_at
        self._next_heartbeat_at = self._now()
        self._healthy = False
        return True

    def _heartbeat(self, health, reason):
        if not self._lease_id:
            return False
        request = capability_pb2.HeartbeatCapabilityProviderRequest(
            lease_id=self._lease_id,
            health=health,
            health_reason=reason,
        )
        try:
            response = self._registry.HeartbeatCapabilityProvider(request, timeout=self._config.rpc_timeout)
        except grpc.RpcError:
            self._clear_lease()
            return False
        expires_at = _response_expiry(response)
        now = self._now()
        if expires_at is None or expires_at <= now:
            self._clear_lease()
            return False
        self._expires_at = expires_at
        self._next_heartbeat_at = now + self._config.heartbeat_interval
        self._healthy = health == capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY
        return self._healthy

    def _publish(self, **payload):
        with self._lock:
            if not self.active:
                return False
            sequence = self.next_source_sequence()
            lease_id = self._lease_id
            observation_id = str(self._uuid_factory())
            occurred_at = timestamp_pb2.Timestamp()
            occurred_at.FromDatetime(self._now())
            ttl = duration_pb2.Duration()
            ttl.FromTimedelta(self._config.observation_ttl)
            envelope = observation_pb2.ObservationEnvelope(
                id=observation_id,
                source_id=self._config.instance_id,
                source_seq=sequence,
                occurred_at=occurred_at,
                ttl=ttl,
                subject_id=self._config.subject_id,
                confidence=1.0,
                trace_id=f"trace:{observation_id}",
                **payload,
            )
        try:
            response = self._ingress.Publish(
                adapter_pb2.PublishRequest(observation=envelope, provider_lease_id=lease_id),
                timeout=self._config.rpc_timeout,
            )
        except grpc.RpcError:
            self._clear_lease_if_current(lease_id)
            return False
        if response is None or not response.HasField("receipt"):
            self._clear_lease_if_current(lease_id)
            return False
        with self._lock:
            if self._closed or self._lease_id != lease_id:
                return False
            return response.receipt.status in (
                adapter_pb2.RECEIPT_STATUS_ACCEPTED,
                adapter_pb2.RECEIPT_STATUS_DUPLICATE,
            )

    def _now(self):
        now = self._utcnow()
        if now.tzinfo is None or now.utcoffset() is None:
            raise ValueError("UTC clock must return timezone-aware values")
        return now.astimezone(UTC)

    def _clear_lease(self):
        self._lease_id = ""
        self._expires_at = None
        self._next_heartbeat_at = None
        self._healthy = False

    def _clear_lease_if_current(self, lease_id):
        with self._lock:
            if self._lease_id == lease_id:
                self._clear_lease()


def _response_expiry(response):
    if response is None or not response.HasField("expires_at"):
        return None
    try:
        return response.expires_at.ToDatetime(tzinfo=UTC)
    except ValueError:
        return None
