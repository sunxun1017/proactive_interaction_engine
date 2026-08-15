"""Pure aggregate biometric curves and explicit threshold selection checks.

The functions accept only anonymous integer scores in memory. They expose no
path, subject, sample, media, embedding, or per-trial score output.
"""

from dataclasses import dataclass, fields


SCORE_SCALE_PPM = 1_000_000
MAX_DURATION_US = 9_223_372_036_854_775


@dataclass(frozen=True)
class FARFRRPoint:
    threshold_ppm: int
    false_accept_count: int
    impostor_trial_count: int
    false_reject_count: int
    genuine_trial_count: int
    far_ppm: int
    frr_ppm: int


@dataclass(frozen=True)
class APCERBPCERPoint:
    threshold_ppm: int
    attack_accept_count: int
    attack_trial_count: int
    bona_fide_reject_count: int
    bona_fide_trial_count: int
    apcer_ppm: int
    bpcer_ppm: int


@dataclass(frozen=True)
class LatencyAggregate:
    sample_count: int
    p95_us: int
    p99_us: int
    observed_maximum_us: int


@dataclass(frozen=True)
class ManualSelection:
    face_detection_threshold_ppm: int
    face_nms_threshold_ppm: int
    face_liveness_threshold_ppm: int
    face_identification_threshold_ppm: int
    speaker_identification_threshold_ppm: int
    speaker_verification_threshold_ppm: int


@dataclass(frozen=True)
class SelectionGrids:
    face_detection: tuple
    face_nms: tuple
    face_liveness: tuple
    face_identification: tuple
    speaker_identification: tuple
    speaker_verification: tuple


def fixed_rate_ppm(numerator, denominator):
    """Return integer ppm using ROUND_HALF_UP_V1 with no floating point."""
    _require_integer(numerator, "rate numerator")
    _require_integer(denominator, "rate denominator")
    if denominator <= 0 or numerator < 0 or numerator > denominator:
        raise ValueError("rate requires 0 <= numerator <= positive denominator")
    return (numerator * SCORE_SCALE_PPM + denominator // 2) // denominator


def aggregate_far_frr(genuine_scores_ppm, impostor_scores_ppm, threshold_grid_ppm):
    """Build aggregate FAR/FRR points; a score equal to threshold is accepted."""
    genuine = _validated_scores(genuine_scores_ppm, "genuine scores")
    impostor = _validated_scores(impostor_scores_ppm, "impostor scores")
    thresholds = _validated_grid(threshold_grid_ppm, "FAR/FRR threshold grid")
    points = []
    for threshold in thresholds:
        false_accepts = sum(score >= threshold for score in impostor)
        false_rejects = sum(score < threshold for score in genuine)
        points.append(
            FARFRRPoint(
                threshold_ppm=threshold,
                false_accept_count=false_accepts,
                impostor_trial_count=len(impostor),
                false_reject_count=false_rejects,
                genuine_trial_count=len(genuine),
                far_ppm=fixed_rate_ppm(false_accepts, len(impostor)),
                frr_ppm=fixed_rate_ppm(false_rejects, len(genuine)),
            )
        )
    return tuple(points)


def aggregate_apcer_bpcer(bona_fide_scores_ppm, attack_scores_ppm, threshold_grid_ppm):
    """Build aggregate liveness APCER/BPCER points for real-score >= threshold."""
    bona_fide = _validated_scores(bona_fide_scores_ppm, "bona fide scores")
    attacks = _validated_scores(attack_scores_ppm, "attack scores")
    thresholds = _validated_grid(threshold_grid_ppm, "liveness threshold grid")
    points = []
    for threshold in thresholds:
        attack_accepts = sum(score >= threshold for score in attacks)
        bona_fide_rejects = sum(score < threshold for score in bona_fide)
        points.append(
            APCERBPCERPoint(
                threshold_ppm=threshold,
                attack_accept_count=attack_accepts,
                attack_trial_count=len(attacks),
                bona_fide_reject_count=bona_fide_rejects,
                bona_fide_trial_count=len(bona_fide),
                apcer_ppm=fixed_rate_ppm(attack_accepts, len(attacks)),
                bpcer_ppm=fixed_rate_ppm(bona_fide_rejects, len(bona_fide)),
            )
        )
    return tuple(points)


def aggregate_latency_us(samples_us):
    """Aggregate positive microseconds with NEAREST_RANK_V1 quantiles."""
    try:
        samples = tuple(samples_us)
    except TypeError as error:
        raise ValueError("latency samples must be an iterable") from error
    if not samples:
        raise ValueError("latency samples must not be empty")
    for sample in samples:
        _require_integer(sample, "latency samples")
        if sample <= 0 or sample > MAX_DURATION_US:
            raise ValueError("latency samples must be positive bounded microseconds")
    ordered = sorted(samples)
    return LatencyAggregate(
        sample_count=len(ordered),
        p95_us=_nearest_rank(ordered, 95, 100),
        p99_us=_nearest_rank(ordered, 99, 100),
        observed_maximum_us=ordered[-1],
    )


def validate_manual_selection(selection, grids):
    """Validate six caller-chosen thresholds without selecting or ranking them."""
    if not isinstance(selection, ManualSelection) or not isinstance(grids, SelectionGrids):
        raise ValueError("manual selection and explicit selection grids are required")
    positive_thresholds = {
        "face_liveness_threshold_ppm",
        "face_identification_threshold_ppm",
        "speaker_identification_threshold_ppm",
        "speaker_verification_threshold_ppm",
    }
    for field in fields(ManualSelection):
        value = getattr(selection, field.name)
        _require_score(value, field.name)
        if field.name in positive_thresholds and value == 0:
            raise ValueError(f"{field.name} must be positive")
        grid_name = field.name.removesuffix("_threshold_ppm")
        grid = _validated_grid(getattr(grids, grid_name), f"{grid_name} grid")
        if value not in grid:
            raise ValueError(f"{field.name} must be an explicitly measured grid point")
    return selection


def _validated_scores(values, name):
    try:
        result = tuple(values)
    except TypeError as error:
        raise ValueError(f"{name} must be an iterable") from error
    if not result:
        raise ValueError(f"{name} must not be empty")
    for value in result:
        _require_score(value, name)
    return result


def _validated_grid(values, name):
    result = _validated_scores(values, name)
    if any(left >= right for left, right in zip(result, result[1:])):
        raise ValueError(f"{name} must be strictly increasing without duplicates")
    return result


def _require_score(value, name):
    _require_integer(value, name)
    if value < 0 or value > SCORE_SCALE_PPM:
        raise ValueError(f"{name} values must be within [0, 1000000] ppm")


def _require_integer(value, name):
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{name} must contain integers")


def _nearest_rank(ordered, numerator, denominator):
    rank = (numerator * len(ordered) + denominator - 1) // denominator
    return ordered[rank - 1]
