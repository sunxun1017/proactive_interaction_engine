from collections import UserDict
import datetime
import threading
import unittest
import uuid

import grpc
from proactive.platform.v1 import adapter_pb2, capability_pb2

from workers.provider import (
    ProviderConfig,
    ProviderConnection,
    ProviderSession,
    parse_provider_bindings,
)


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
        self.assertTrue(self.session.publish_presence(False))
        self.registry.heartbeat_error = FakeRPCError(grpc.StatusCode.NOT_FOUND)

        self.now += datetime.timedelta(seconds=5)
        self.assertFalse(self.session.maintain())
        self.assertFalse(self.session.publish_presence(True))
        self.assertEqual(len(self.ingress.publishes), 1)

        self.registry.heartbeat_error = None
        self.assertTrue(self.session.maintain())
        self.assertEqual(len(self.registry.registers), 2)
        self.assertTrue(self.session.publish_presence(True))
        self.assertEqual(self.ingress.publishes[-1][0].provider_lease_id, "lease-2")
        self.assertEqual(self.ingress.publishes[-1][0].observation.source_seq, 2)

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

    def test_unhealthy_desired_state_survives_heartbeat_and_reregistration(self):
        self.assertFalse(self.session.mark_unhealthy("MODEL_UNAVAILABLE"))
        self.assertFalse(self.session.start())
        self.assertTrue(self.session.leased)
        self.assertFalse(self.session.active)
        self.assertEqual(
            self.registry.heartbeats[-1][0].health_reason,
            capability_pb2.PROVIDER_HEALTH_REASON_MODEL_UNAVAILABLE,
        )

        self.now += datetime.timedelta(seconds=15)
        self.registry.now = self.now
        self.assertFalse(self.session.maintain())

        self.assertEqual(len(self.registry.registers), 2)
        heartbeat = self.registry.heartbeats[-1][0]
        self.assertEqual(heartbeat.health, capability_pb2.PROVIDER_HEALTH_STATE_UNHEALTHY)
        self.assertEqual(heartbeat.health_reason, capability_pb2.PROVIDER_HEALTH_REASON_MODEL_UNAVAILABLE)

    def test_independent_sessions_share_connection_but_not_lease_or_sequence(self):
        channel = FakeChannel()
        connection = ProviderConnection(channel, self.registry, self.ingress)
        face = connection.session(
            provider_config("face", capability_pb2.SERVICE_CAPABILITY_KIND_FACE_DETECTION),
            utcnow=lambda: self.now,
        )
        speaker = connection.session(
            provider_config("speaker", capability_pb2.SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION),
            utcnow=lambda: self.now,
        )

        self.assertTrue(face.start())
        self.assertTrue(speaker.start())
        self.assertNotEqual(face.lease_id, speaker.lease_id)
        self.assertEqual(face.next_source_sequence(), 1)
        self.assertEqual(face.next_source_sequence(), 2)
        self.assertEqual(speaker.next_source_sequence(), 1)

        face.close()
        speaker.close()
        self.assertEqual(channel.closed, 0)
        connection.close()
        connection.close()
        self.assertEqual(channel.closed, 1)

    def test_health_reasons_are_complete_and_invalid_pairs_are_rejected(self):
        self.session.start()
        for reason in (
            "DEVICE_UNAVAILABLE",
            "PERMISSION_DENIED",
            "DEPENDENCY_UNAVAILABLE",
            "MODEL_UNAVAILABLE",
            "INTERNAL_ERROR",
            "SHUTTING_DOWN",
        ):
            with self.subTest(reason=reason):
                self.session.mark_unhealthy(reason)
                self.assertEqual(
                    capability_pb2.ProviderHealthReason.Name(
                        self.registry.heartbeats[-1][0].health_reason
                    ),
                    "PROVIDER_HEALTH_REASON_" + reason,
                )
        with self.assertRaises(ValueError):
            self.session.mark_unhealthy("UNKNOWN_REASON")

    def test_publish_failure_cannot_clear_a_concurrently_replaced_lease(self):
        self.session.start()
        self.ingress.block = threading.Event()
        self.ingress.entered = threading.Event()
        self.ingress.error = FakeRPCError(grpc.StatusCode.UNAVAILABLE)
        result = []
        thread = threading.Thread(target=lambda: result.append(self.session.publish_presence(True)))
        thread.start()
        self.assertTrue(self.ingress.entered.wait(1))

        self.now += datetime.timedelta(seconds=15)
        self.registry.now = self.now
        self.assertTrue(self.session.maintain())
        self.assertEqual(self.session.lease_id, "lease-2")
        self.ingress.block.set()
        thread.join(1)

        self.assertEqual(result, [False])
        self.assertEqual(self.session.lease_id, "lease-2")
        self.assertTrue(self.session.active)

    def test_close_racing_with_publish_never_reports_success(self):
        self.session.start()
        self.ingress.block = threading.Event()
        self.ingress.entered = threading.Event()
        result = []
        thread = threading.Thread(target=lambda: result.append(self.session.publish_presence(True)))
        thread.start()
        self.assertTrue(self.ingress.entered.wait(1))

        self.session.close()
        self.ingress.block.set()
        thread.join(1)

        self.assertEqual(result, [False])
        self.assertFalse(self.session.active)


class ProviderBindingEnvironmentTest(unittest.TestCase):
    def test_parses_exact_sorted_vision_subset(self):
        bindings = parse_provider_bindings(
            {
                "PROACTIVE_PROVIDER_COUNT": "2",
                "PROACTIVE_PROVIDER_0_ID": "desktop-presence",
                "PROACTIVE_PROVIDER_0_CAPABILITY": "PERSON_PRESENCE",
                "PROACTIVE_PROVIDER_1_ID": "local-face-detection",
                "PROACTIVE_PROVIDER_1_CAPABILITY": "FACE_DETECTION",
                "PATH": "/bin",
            },
            "vision",
        )
        self.assertEqual(
            [(item.provider_id, item.capability_name) for item in bindings],
            [
                ("desktop-presence", "PERSON_PRESENCE"),
                ("local-face-detection", "FACE_DETECTION"),
            ],
        )

    def test_accepts_os_environ_like_mapping_with_string_entries(self):
        environment = UserDict({
            "PROACTIVE_PROVIDER_COUNT": "1",
            "PROACTIVE_PROVIDER_0_ID": "desktop-vad",
            "PROACTIVE_PROVIDER_0_CAPABILITY": "VOICE_ACTIVITY",
        })
        self.assertEqual(parse_provider_bindings(environment, "audio")[0].provider_id, "desktop-vad")
        environment["NOT_A_STRING_VALUE"] = 1
        with self.assertRaises(ValueError):
            parse_provider_bindings(environment, "audio")

    def test_rejects_missing_extra_unsorted_duplicate_or_wrong_stack_bindings(self):
        base = {
            "PROACTIVE_PROVIDER_COUNT": "2",
            "PROACTIVE_PROVIDER_0_ID": "desktop-vad",
            "PROACTIVE_PROVIDER_0_CAPABILITY": "VOICE_ACTIVITY",
            "PROACTIVE_PROVIDER_1_ID": "local-speaker-identity",
            "PROACTIVE_PROVIDER_1_CAPABILITY": "SPEAKER_IDENTIFICATION",
        }
        invalid = []
        missing = dict(base)
        del missing["PROACTIVE_PROVIDER_1_CAPABILITY"]
        invalid.append(missing)
        extra = dict(base)
        extra["PROACTIVE_PROVIDER_2_ID"] = "extra"
        invalid.append(extra)
        unsorted = dict(base)
        unsorted["PROACTIVE_PROVIDER_0_ID"], unsorted["PROACTIVE_PROVIDER_1_ID"] = (
            unsorted["PROACTIVE_PROVIDER_1_ID"],
            unsorted["PROACTIVE_PROVIDER_0_ID"],
        )
        invalid.append(unsorted)
        duplicate = dict(base)
        duplicate["PROACTIVE_PROVIDER_1_CAPABILITY"] = "VOICE_ACTIVITY"
        invalid.append(duplicate)
        wrong_stack = dict(base)
        wrong_stack["PROACTIVE_PROVIDER_1_CAPABILITY"] = "FACE_DETECTION"
        invalid.append(wrong_stack)
        missing_base = dict(base)
        missing_base["PROACTIVE_PROVIDER_0_CAPABILITY"] = "SPEAKER_VERIFICATION"
        invalid.append(missing_base)
        noncanonical_count = dict(base)
        noncanonical_count["PROACTIVE_PROVIDER_COUNT"] = "02"
        invalid.append(noncanonical_count)

        for environment in invalid:
            with self.subTest(environment=environment):
                with self.assertRaises(ValueError):
                    parse_provider_bindings(environment, "audio")

    def test_rejects_reserved_prefix_even_when_count_is_zero_or_stack_unknown(self):
        with self.assertRaises(ValueError):
            parse_provider_bindings({"PROACTIVE_PROVIDER_COUNT": "0"}, "vision")
        with self.assertRaises(ValueError):
            parse_provider_bindings(
                {
                    "PROACTIVE_PROVIDER_COUNT": "1",
                    "PROACTIVE_PROVIDER_0_ID": "desktop-presence",
                    "PROACTIVE_PROVIDER_0_CAPABILITY": "PERSON_PRESENCE",
                    "PROACTIVE_PROVIDER_DEBUG": "1",
                },
                "vision",
            )
        with self.assertRaises(ValueError):
            parse_provider_bindings({}, "unknown")


def provider_config(provider_id, capability):
    return ProviderConfig(
        provider_id=provider_id,
        instance_id="shared-instance",
        capability=capability,
        implementation_version="fake.v1",
        privacy_class=capability_pb2.PROVIDER_PRIVACY_CLASS_DEVICE_LOCAL,
        maximum_latency=datetime.timedelta(seconds=1),
        cancellation_semantics=capability_pb2.PROVIDER_CANCELLATION_SEMANTICS_COOPERATIVE,
        device_requirements=(capability_pb2.PROVIDER_DEVICE_CLASS_CAMERA,),
        subject_id="user-1",
        observation_ttl=datetime.timedelta(seconds=1),
    )


class FakeChannel:
    def __init__(self):
        self.closed = 0

    def close(self):
        self.closed += 1


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
        self.block = None
        self.entered = None
        self.error = None

    def Publish(self, request, timeout):
        self.publishes.append((request, timeout))
        if self.entered is not None:
            self.entered.set()
        if self.block is not None:
            self.block.wait(1)
        if self.error is not None:
            raise self.error
        return adapter_pb2.PublishResponse(
            receipt=adapter_pb2.ObservationReceipt(
                observation_id=request.observation.id,
                status=self.status,
            )
        )


if __name__ == "__main__":
    unittest.main()
