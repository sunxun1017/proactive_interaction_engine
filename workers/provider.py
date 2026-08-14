"""Shared leased provider session for local media workers."""

from dataclasses import dataclass
import datetime
import uuid

import grpc
from google.protobuf import duration_pb2, timestamp_pb2
from proactive.platform.v1 import adapter_pb2, capability_pb2, observation_pb2


UTC = datetime.timezone.utc


@dataclass(frozen=True)
class ProviderConfig:
    provider_id: str
    instance_id: str
    capability: int
    implementation_version: str
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
        if self.observation_ttl <= datetime.timedelta(0):
            raise ValueError("observation TTL must be positive")
        if self.heartbeat_interval <= datetime.timedelta(0) or self.rpc_timeout <= 0:
            raise ValueError("heartbeat interval and RPC timeout must be positive")


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

    @classmethod
    def connect(cls, address, config):
        if not address or address.strip() != address:
            raise ValueError("gRPC address is required")
        channel = grpc.insecure_channel(address)
        from proactive.platform.v1 import adapter_pb2_grpc, capability_pb2_grpc

        return cls(
            config,
            capability_pb2_grpc.CapabilityProviderRegistryServiceStub(channel),
            adapter_pb2_grpc.ObservationIngressServiceStub(channel),
            channel=channel,
        )

    @property
    def active(self):
        if self._closed or not self._healthy or not self._lease_id or self._expires_at is None:
            return False
        return self._now() < self._expires_at

    def start(self):
        if self._closed or not self._register():
            return False
        return self._heartbeat(
            capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY,
            capability_pb2.PROVIDER_HEALTH_REASON_NONE,
        )

    def maintain(self):
        if self._closed:
            return False
        now = self._now()
        if not self._lease_id or self._expires_at is None or now >= self._expires_at:
            self._clear_lease()
            return self.start()
        if self._next_heartbeat_at is not None and now >= self._next_heartbeat_at:
            return self._heartbeat(
                capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY,
                capability_pb2.PROVIDER_HEALTH_REASON_NONE,
            )
        return self.active

    def mark_unhealthy(self, reason):
        reasons = {
            "DEVICE_UNAVAILABLE": capability_pb2.PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE,
            "DEPENDENCY_UNAVAILABLE": capability_pb2.PROVIDER_HEALTH_REASON_DEPENDENCY_UNAVAILABLE,
            "INTERNAL_ERROR": capability_pb2.PROVIDER_HEALTH_REASON_INTERNAL_ERROR,
            "SHUTTING_DOWN": capability_pb2.PROVIDER_HEALTH_REASON_SHUTTING_DOWN,
        }
        mapped = reasons.get(reason)
        if mapped is None or not self._lease_id:
            return False
        return self._heartbeat(capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY, mapped)

    def publish_presence(self, present):
        return self._publish(person_presence=observation_pb2.PersonPresence(present=bool(present)))

    def publish_speech_activity(self):
        return self._publish(speech_activity=observation_pb2.SpeechActivity(active=True))

    def close(self):
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
        request = capability_pb2.RegisterCapabilityProviderRequest(
            provider_id=self._config.provider_id,
            instance_id=self._config.instance_id,
            protocol_version="v1",
            implementation_version=self._config.implementation_version,
            capabilities=[self._config.capability],
            health=capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY,
            health_reason=capability_pb2.PROVIDER_HEALTH_REASON_STARTING,
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
        if not self.active:
            return False
        self._source_seq += 1
        observation_id = str(self._uuid_factory())
        occurred_at = timestamp_pb2.Timestamp()
        occurred_at.FromDatetime(self._now())
        ttl = duration_pb2.Duration()
        ttl.FromTimedelta(self._config.observation_ttl)
        envelope = observation_pb2.ObservationEnvelope(
            id=observation_id,
            source_id=self._config.instance_id,
            source_seq=self._source_seq,
            occurred_at=occurred_at,
            ttl=ttl,
            subject_id=self._config.subject_id,
            confidence=1.0,
            trace_id=f"trace:{observation_id}",
            **payload,
        )
        try:
            response = self._ingress.Publish(
                adapter_pb2.PublishRequest(observation=envelope, provider_lease_id=self._lease_id),
                timeout=self._config.rpc_timeout,
            )
        except grpc.RpcError:
            self._clear_lease()
            return False
        return response is not None and response.HasField("receipt")

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


def _response_expiry(response):
    if response is None or not response.HasField("expires_at"):
        return None
    try:
        return response.expires_at.ToDatetime(tzinfo=UTC)
    except ValueError:
        return None
