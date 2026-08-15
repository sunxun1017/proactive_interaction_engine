import dataclasses
import json
from pathlib import Path
import unittest

from calibration.identity.aggregate import (
    ManualSelection,
    SelectionGrids,
    aggregate_apcer_bpcer,
    aggregate_far_frr,
    aggregate_latency_us,
    fixed_rate_ppm,
    validate_manual_selection,
)
from calibration.identity.artifact import canonical_json_hash, load_strict_json


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
TESTDATA = REPOSITORY_ROOT / "adapters/config/identitycalibration/testdata"


class FARFRRAggregationTest(unittest.TestCase):
    def test_aggregates_counts_and_half_up_ppm_without_sample_output(self):
        points = aggregate_far_frr(
            genuine_scores_ppm=(900_000, 800_000, 700_000),
            impostor_scores_ppm=(800_000, 600_000),
            threshold_grid_ppm=(700_000, 800_000, 900_000),
        )

        selected = points[1]
        self.assertEqual(selected.false_accept_count, 1)
        self.assertEqual(selected.false_reject_count, 1)
        self.assertEqual(selected.impostor_trial_count, 2)
        self.assertEqual(selected.genuine_trial_count, 3)
        self.assertEqual(selected.far_ppm, 500_000)
        self.assertEqual(selected.frr_ppm, 333_333)
        self.assertEqual(
            set(dataclasses.asdict(selected)),
            {
                "threshold_ppm",
                "false_accept_count",
                "impostor_trial_count",
                "false_reject_count",
                "genuine_trial_count",
                "far_ppm",
                "frr_ppm",
            },
        )

    def test_uses_one_fixed_integer_rounding_rule(self):
        self.assertEqual(fixed_rate_ppm(0, 6), 0)
        self.assertEqual(fixed_rate_ppm(1, 6), 166_667)
        self.assertEqual(fixed_rate_ppm(2, 3), 666_667)
        self.assertEqual(fixed_rate_ppm(6, 6), 1_000_000)

    def test_rejects_empty_unsorted_or_non_integer_inputs(self):
        cases = (
            ((), (1,), (1,)),
            ((1,), (), (1,)),
            ((1,), (1,), ()),
            ((1,), (1,), (2, 1)),
            ((1,), (1,), (1, 1)),
            ((True,), (1,), (1,)),
            ((1.0,), (1,), (1,)),
            ((1_000_001,), (1,), (1,)),
        )
        for genuine, impostor, grid in cases:
            with self.subTest(genuine=genuine, impostor=impostor, grid=grid):
                with self.assertRaises(ValueError):
                    aggregate_far_frr(genuine, impostor, grid)


class LivenessAggregationTest(unittest.TestCase):
    def test_aggregates_apcer_and_bpcer_instead_of_mislabeling_far_frr(self):
        points = aggregate_apcer_bpcer(
            bona_fide_scores_ppm=(900_000, 700_000),
            attack_scores_ppm=(800_000, 600_000),
            threshold_grid_ppm=(700_000, 800_000),
        )

        selected = points[0]
        self.assertEqual(selected.attack_accept_count, 1)
        self.assertEqual(selected.bona_fide_reject_count, 0)
        self.assertEqual(selected.apcer_ppm, 500_000)
        self.assertEqual(selected.bpcer_ppm, 0)
        serialized = json.dumps([dataclasses.asdict(point) for point in points])
        for forbidden in ("path", "subject", "sample", "media", "embedding", "score_ppm"):
            self.assertNotIn(forbidden, serialized)


class LatencyAggregationTest(unittest.TestCase):
    def test_uses_nearest_rank_for_single_small_and_full_percentile_sets(self):
        single = aggregate_latency_us((17,))
        self.assertEqual(
            dataclasses.asdict(single),
            {"sample_count": 1, "p95_us": 17, "p99_us": 17, "observed_maximum_us": 17},
        )

        small = aggregate_latency_us((20, 10))
        self.assertEqual((small.p95_us, small.p99_us, small.observed_maximum_us), (20, 20, 20))

        full = aggregate_latency_us(tuple(range(100, 0, -1)))
        self.assertEqual((full.sample_count, full.p95_us, full.p99_us, full.observed_maximum_us), (100, 95, 99, 100))

        maximum = aggregate_latency_us((9_223_372_036_854_775,))
        self.assertEqual(maximum.observed_maximum_us, 9_223_372_036_854_775)

    def test_rejects_empty_non_integer_non_positive_or_unbounded_samples(self):
        cases = ((), (True,), (1.0,), (0,), (-1,), (9_223_372_036_854_776,))
        for samples in cases:
            with self.subTest(samples=samples), self.assertRaises(ValueError):
                aggregate_latency_us(samples)


class ManualSelectionTest(unittest.TestCase):
    def test_requires_every_explicit_selection_to_exist_in_its_measured_grid(self):
        grids = SelectionGrids(
            face_detection=(600_000, 650_000),
            face_nms=(300_000, 400_000),
            face_liveness=(700_000, 750_000),
            face_identification=(800_000, 850_000),
            speaker_identification=(750_000, 780_000),
            speaker_verification=(800_000, 830_000),
        )
        selection = ManualSelection(
            face_detection_threshold_ppm=650_000,
            face_nms_threshold_ppm=300_000,
            face_liveness_threshold_ppm=700_000,
            face_identification_threshold_ppm=800_000,
            speaker_identification_threshold_ppm=780_000,
            speaker_verification_threshold_ppm=830_000,
        )

        self.assertEqual(validate_manual_selection(selection, grids), selection)

        invalid = dataclasses.replace(selection, speaker_verification_threshold_ppm=820_000)
        with self.assertRaises(ValueError):
            validate_manual_selection(invalid, grids)

        zero_grid = dataclasses.replace(grids, face_identification=(0, 800_000))
        zero_application_threshold = dataclasses.replace(selection, face_identification_threshold_ppm=0)
        with self.assertRaises(ValueError):
            validate_manual_selection(zero_application_threshold, zero_grid)

    def test_selection_has_no_defaults_or_automatic_eer_path(self):
        with self.assertRaises(TypeError):
            ManualSelection()
        import calibration.identity.aggregate as aggregate

        self.assertFalse(hasattr(aggregate, "select_eer"))
        self.assertFalse(hasattr(aggregate, "select_best_threshold"))


class SharedArtifactGoldenTest(unittest.TestCase):
    def test_strict_json_hash_matches_go_golden(self):
        document = load_strict_json((TESTDATA / "synthetic-valid.v1.json").read_bytes())
        expected = (TESTDATA / "synthetic-valid.v1.sha256").read_text(encoding="ascii").strip()

        self.assertEqual(canonical_json_hash(document), expected)

    def test_strict_json_rejects_duplicates_null_floats_and_trailing_values(self):
        cases = (
            b'{"schema_version":1,"schema_version":1}',
            b'{"value":null}',
            b'{"value":1.0}',
            b'{"value":"1"}{}',
        )
        for content in cases:
            with self.subTest(content=content), self.assertRaises(ValueError):
                load_strict_json(content)


if __name__ == "__main__":
    unittest.main()
