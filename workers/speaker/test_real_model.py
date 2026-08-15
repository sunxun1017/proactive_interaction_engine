"""Opt-in conformance against pinned official examples; never downloads assets."""

import json
import os
import pathlib
import unittest

from workers.speaker.conformance import run_conformance


@unittest.skipUnless(os.environ.get("PIE_SPEAKER_REAL_MODEL") == "1", "real speaker model is opt-in")
class RealSpeakerModelConformanceTest(unittest.TestCase):
    def test_official_model_examples_on_explicit_cuda_device(self):
        required = (
            "PIE_SPEAKER_CHECKPOINT",
            "PIE_SPEAKER_EXAMPLE_SAME_A",
            "PIE_SPEAKER_EXAMPLE_SAME_B",
            "PIE_SPEAKER_EXAMPLE_DIFFERENT",
            "PIE_SPEAKER_CUDA_DEVICE",
        )
        missing = [name for name in required if not os.environ.get(name)]
        self.assertFalse(missing, f"missing conformance environment: {missing}")

        report = run_conformance(
            checkpoint_path=pathlib.Path(os.environ["PIE_SPEAKER_CHECKPOINT"]),
            same_a=pathlib.Path(os.environ["PIE_SPEAKER_EXAMPLE_SAME_A"]),
            same_b=pathlib.Path(os.environ["PIE_SPEAKER_EXAMPLE_SAME_B"]),
            different=pathlib.Path(os.environ["PIE_SPEAKER_EXAMPLE_DIFFERENT"]),
            device=os.environ["PIE_SPEAKER_CUDA_DEVICE"],
            iterations=5,
        )

        self.assertGreater(report["scores"]["same_speaker"], report["scores"]["different_speaker"])
        self.assertLessEqual(report["determinism_max_abs_delta"], 1e-5)
        print(json.dumps(report, sort_keys=True))


if __name__ == "__main__":
    unittest.main()
