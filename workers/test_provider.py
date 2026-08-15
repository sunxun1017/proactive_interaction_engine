import datetime
import unittest
import uuid

import grpc
from proactive.platform.v1 import adapter_pb2, capability_pb2

from workers.provider import ProviderConfig, ProviderSession


UTC = datetime.timezone.utc


class ProviderSessionTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 8, 14, 8, 0, tzinfo=UTC)
        self.registry = FakeRegistry(self.now)
        self.ingress = FakeIngress()
        identifiers = iter(
            [
                uuid.UUID("00000000-0000-0000-0000-000000000001"),
                uuid.UUID("00000000-0000-0000-0000-000000000002"),
            ]
        )
        self.session = ProviderSession(
            ProviderConfig(
                provider_id="desktop-presence",
                instance_id="presence-worker-1",
                capability=capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
                implementation_version="camera.v1",
                privacy_class=capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
                maximum_latency=datetime.timedelta(seconds=3),
                cancellation_semantics=capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
                device_requirements=(capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,),
                subject_id="user-1",
                observation_ttl=datetime.timedelta(seconds=2),
                heartbeat_interval=datetime.timedelta(seconds=5),
                rpc_timeout=1.5,
            ),
            self.registry,
            self.ingress,
            utcnow=lambda: self.now,
            uuid_factory=lambda: next(identifiers),
        )

    def test_start_registers_starting_then_heartbeats_healthy(self):
        self.assertTrue(self.session.start())

        registration = self.registry.registers[0][0]
        self.assertEqual(registration.provider_id, "desktop-presence")
        self.assertEqual(registration.instance_id, "presence-worker-1")
        self.assertEqual(registration.protocol_version, "v1")
        self.assertEqual(registration.capabilities, [capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE])
        self.assertEqual(
            registration.operational_profile.privacy_class,
            capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
        )
        self.assertEqual(
            registration.operational_profile.maximum_latency.ToTimedelta(),
            datetime.timedelta(seconds=3),
        )
        self.assertEqual(
            registration.operational_profile.cancellation_semantics,
            capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
        )
        self.assertEqual(
            registration.operational_profile.device_requirements,
            [capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA],
        )
        self.assertEqual(registration.health, capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY)
        self.assertEqual(registration.health_reason, capability_pb2.PROVIDER_HEALTH_REASON_STARTING)
        heartbeat = self.registry.heartbeats[0][0]
        self.assertEqual(heartbeat.health, capability_pb2.PROVIDER_HEALTH_STATE_HEALTHY)
        self.assertEqual(heartbeat.health_reason, capability_pb2.PROVIDER_HEALTH_REASON_NONE)
        self.assertTrue(self.session.active)

    def test_config_rejects_invalid_operational_profile(self):
        valid = dict(
            provider_id="provider",
            instance_id="instance",
            capability=capability_pb2.SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
            implementation_version="implementation.v1",
            privacy_class=capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
            maximum_latency=datetime.timedelta(seconds=1),
            cancellation_semantics=capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
            device_requirements=(capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,),
            subject_id="user-1",
            observation_ttl=datetime.timedelta(seconds=1),
        )
        invalid = (
            ("privacy_class", capability_pb2.PROVIDER_PRIVACY_CLASS_UNSPECIFIED),
            ("maximum_latency", datetime.timedelta(0)),
            (
                "cancellation_semantics",
                capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_UNSPECIFIED,
            ),
            (
                "device_requirements",
                (capability_pb2.PROVIDER_DEVICE_CLASS_UNSPECIFIED,),
            ),
            (
                "device_requirements",
                (
                    capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,
                    capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,
                ),
            ),
        )
        for field, value in invalid:
            with self.subTest(field=field, value=value):
                config = dict(valid)
                config[field] = value
                with self.assertRaises(ValueError):
                    ProviderConfig(**config)

    def test_publish_uses_monotonic_sequence_uuid_utc_ttl_and_lease(self):
        self.session.start()

        self.assertTrue(self.session.publish_presence(True))
        self.assertTrue(self.session.publish_presence(False))

        requests = [call[0] for call in self.ingress.publishes]
        self.assertEqual([request.provider_lease_id for request in requests], ["lease-1", "lease-1"])
        self.assertEqual([request.observation.source_seq for request in requests], [1, 2])
        self.assertEqual(
            [request.observation.id for request in requests],
            [
                "00000000-0000-0000-0000-000000000001",
                "00000000-0000-0000-0000-000000000002",
            ],
        )
        envelope = requests[0].observation
        self.assertEqual(envelope.source_id, "presence-worker-1")
        self.assertEqual(envelope.subject_id, "user-1")
        self.assertEqual(envelope.occurred_at.ToDatetime(tzinfo=UTC), self.now)
        self.assertEqual(envelope.ttl.ToTimedelta(), datetime.timedelta(seconds=2))
        self.assertTrue(envelope.person_presence.present)

    def test_lost_lease_suppresses_publish_until_reregistered(self):
        self.session.start()
        self.registry.heartbeat_error = FakeRPCError(grpc.StatusCode.NOT_FOUND)

        self.now += datetime.timedelta(seconds=5)
        self.assertFalse(self.session.maintain())
        self.assertFalse(self.session.publish_presence(True))
        self.assertEqual(self.ingress.publishes, [])

        self.registry.heartbeat_error = None
        self.assertTrue(self.session.maintain())
        self.assertEqual(len(self.registry.registers), 2)
        self.assertTrue(self.session.publish_presence(True))
        self.assertEqual(self.ingress.publishes[0][0].provider_lease_id, "lease-2")

    def test_close_uses_bounded_unregister_and_disables_future_publish(self):
        self.session.start()

        self.session.close()

        self.assertEqual(self.registry.unregisters[0][0].lease_id, "lease-1")
        self.assertEqual(self.registry.unregisters[0][1], 1.5)
        self.assertFalse(self.session.publish_presence(True))

    def test_rejected_receipt_is_handled_without_claiming_publish_success(self):
        self.session.start()
        self.ingress.status = adapter_pb2.RECEIPT_STATUS_REJECTED

        self.assertFalse(self.session.publish_presence(True))
        self.assertTrue(self.session.active)


class FakeRPCError(grpc.RpcError):
    def __init__(self, status_code):
        super().__init__()
        self._status_code = status_code

    def code(self):
        return self._status_code


class FakeRegistry:
    def __init__(self, now):
        self.now = now
        self.registers = []
        self.heartbeats = []
        self.unregisters = []
        self.heartbeat_error = None

    def RegisterCapabilityProvider(self, request, timeout):
        self.registers.append((request, timeout))
        response = capability_pb2.RegisterCapabilityProviderResponse(lease_id=f"lease-{len(self.registers)}")
        response.expires_at.FromDatetime(self.now + datetime.timedelta(seconds=15))
        return response

    def HeartbeatCapabilityProvider(self, request, timeout):
        self.heartbeats.append((request, timeout))
        if self.heartbeat_error is not None:
            raise self.heartbeat_error
        response = capability_pb2.HeartbeatCapabilityProviderResponse()
        response.expires_at.FromDatetime(self.now + datetime.timedelta(seconds=15))
        return response

    def UnregisterCapabilityProvider(self, request, timeout):
        self.unregisters.append((request, timeout))
        return capability_pb2.UnregisterCapabilityProviderResponse()


class FakeIngress:
    def __init__(self):
        self.publishes = []
        self.status = adapter_pb2.RECEIPT_STATUS_ACCEPTED

    def Publish(self, request, timeout):
        self.publishes.append((request, timeout))
        return adapter_pb2.PublishResponse(
            receipt=adapter_pb2.ObservationReceipt(
                observation_id=request.observation.id,
                status=self.status,
            )
        )


if __name__ == "__main__":
    unittest.main()
