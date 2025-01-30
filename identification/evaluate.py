import argparse
import json
import subprocess
from enum import Enum
from pathlib import Path
from typing import Any, NamedTuple

import numpy as np
import pandas as pd

from datasets import load_tsv_datasets, create_bursts, create_bursts_unknown
from models import (
    KDCandidatePredictor,
    KDPointsTransformer,
    CandidateScoreTransformer,
    CandidateSelector,
)


class Model(Enum):
    STRONG = "strong"
    WEAK = "weak"
    UNKNOWN = "unknown"

    def __str__(self):
        return self.value


class Method(Enum):
    NETWORK = "network"
    WIFI = "wifi"

    def __str__(self):
        return self.value


class Setting(NamedTuple):
    models: list[Model]
    methods: list[Method]
    recompute: bool
    theta: float
    time: int
    burstshark_path: Path


def main() -> None:
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "--model", type=Model, choices=list(Model), help="Attack model to evaluate (default: all)"
    )
    parser.add_argument(
        "--method",
        type=Method,
        choices=list(Method),
        help="Attack method to evaluate (default: all)",
    )
    parser.add_argument(
        "--theta",
        type=float,
        default=2.2,
        help="Confidence threshold (θ) for selecting candidates (default: 2.2)",
    )
    parser.add_argument(
        "--time",
        type=int,
        choices=range(1, 601),
        default=600,
        metavar="[1-600]",
        help="Playback time (t) in seconds to consider (default: 600)",
    )
    parser.add_argument(
        "--recompute",
        action=argparse.BooleanOptionalAction,
        type=bool,
        default=False,
        help="By default, precomputed data is used. Set this flag to reproduce the results using the TSV dataset and the raw PCAP files. Requires burstshark (and by extension tshark) to be installed. Recommended minimum memory: 192 GB.",
    )
    parser.add_argument(
        "--burstshark-path",
        type=Path,
        default=Path("./burstshark-x86_64-linux"),
        help="Path to the burstshark binary. Only required if '--recompute' is set (default: ./burstshark-x86_64-linux)",
    )

    args = parser.parse_args()

    if args.model == Model.STRONG and args.method == Method.WIFI:
        parser.error("Model 'strong' and method 'wifi' are incompatible")

    if args.recompute:
        try:
            completed = subprocess.run(
                ["tshark", "--version"], capture_output=True, text=True, check=True
            )
            if "3.6.2" not in completed.stdout.split("\n", 1)[0]:
                print("tshark version differs from 3.6.2 used in experiments, but likely OK anyway", flush=True)
        except (subprocess.CalledProcessError, FileNotFoundError):
            parser.error("tshark is required with '--recompute'")

        try:
            subprocess.run([args.burstshark_path.resolve(), "--version"], capture_output=True, check=True)
        except (subprocess.CalledProcessError, FileNotFoundError):
            parser.error(
                "burstshark is required with '--recompute'. If the included binary does not work, it may be compiled manually and its path provided with '--burstshark-path'"
            )

    models = [args.model] if args.model is not None else list(Model)
    methods = [args.method] if args.method is not None else list(Method)
    setting = Setting(models, methods, args.recompute, args.theta, args.time, args.burstshark_path.resolve())

    run(setting)


def load_testing_set() -> dict[str, Any]:
    with Path("./testing_set.json").open("r", encoding="utf-8") as f:
        testing_set = json.load(f)

    return testing_set


def load_dataset(setting: Setting) -> tuple[pd.DataFrame, pd.DataFrame]:
    print("Loading dataset...", end="", flush=True)
    if setting.recompute:
        df_videos, df_representations = load_tsv_datasets()
    else:
        df_videos = pd.read_parquet("./precomputed/tsv/videos.parquet")
        df_representations = pd.read_parquet("./precomputed/tsv/representations.parquet")
    print("\rLoading dataset... DONE", flush=True)

    return df_videos, df_representations


def load_kd(
    df_representations: pd.DataFrame, setting: Setting
) -> tuple[KDPointsTransformer, dict[str, KDCandidatePredictor]]:
    X = df_representations["segment_sizes"].to_numpy()
    y = df_representations.index.to_numpy(dtype="uint32")

    kdp = KDPointsTransformer().fit(X)
    kdt = {}
    if setting.recompute:
        for service, ind in df_representations.groupby("service").indices.items():
            print(f"Building k-d tree for {service}...", end="", flush=True)
            kdt[service] = KDCandidatePredictor().fit(kdp.transform(X[ind]), y[ind])
            print(f"\rBuilding k-d tree for {service}... DONE", flush=True)

    return kdp, kdt


def run(setting: Setting) -> None:
    df_videos, df_representations = load_dataset(setting)
    kdp, kdt = load_kd(df_representations, setting)

    for model in setting.models:
        if model in (Model.STRONG, Model.WEAK):
            for method in setting.methods:
                if model == Model.STRONG and method == Method.WIFI:
                    continue

                run_main(df_videos, df_representations, kdp, kdt, model, method, setting)
        else:
            run_unknown(kdp, kdt, setting)


def run_main(
    df_videos: pd.DataFrame,
    df_representations: pd.DataFrame,
    kdp: KDPointsTransformer,
    kdt: dict[str, KDCandidatePredictor],
    model: Model,
    method: Method,
    setting: Setting,
) -> None:
    print(f"Evaluating model '{model}' with method '{method}'...", end="", flush=True)

    testing_set = load_testing_set()
    if model == Model.STRONG:
        testing_set = [session for session in testing_set if not session["vpn"]]
    else:
        testing_set = [session for session in testing_set if session["vpn"]]

    num_playbacks = sum(len(session["playbacks"]) for session in testing_set)
    df_results = pd.DataFrame(
        {
            col: np.zeros(num_playbacks, dtype=dtype)
            for col, dtype in {
                "device": "str",
                "service": "str",
                "video": "str",
                "identified": "int32",
                "unknown": "int32",
                "misidentified": "int32",
                "identified_time": "float64",
            }.items()
        },
    )
    row_idx = 0

    for session in testing_set:
        identifier, _ = session[f"{method}_capture_file"].split(".")

        if setting.recompute:
            txt = create_bursts(session, setting.burstshark_path, method == Method.WIFI)
        else:
            txt = Path(f"./precomputed/bursts/{identifier}.txt")
        data = np.loadtxt(
            txt,
            dtype=[
                ("times", "float64"),
                ("bursts", "int32"),
                ("services", "U6"),
                ("video_ids", "U49"),
            ],
        )
        times, bursts, services, video_ids = (
            data["times"],
            data["bursts"],
            data["services"],
            data["video_ids"],
        )

        if setting.recompute:
            if model == Model.STRONG:
                # As the strong attacker we can query each service tree individually.
                unique_services, indices = np.unique(services, return_index=True)
                service_kd_pred = []
                for service in unique_services[np.argsort(indices)]:
                    kd_pred = kdt[service].predict(
                        kdp.transform([bursts[np.where(services == service)]])
                    )[0]
                    service_kd_pred.append(kd_pred)

                i_and_ell = np.concatenate([kd_pred[0] for kd_pred in service_kd_pred], axis=0)
                distances = np.concatenate([kd_pred[1] for kd_pred in service_kd_pred], axis=0)
            else:
                # As the weak attacker we don't know the service and have to query all trees.
                service_kd_pred = []
                for service in df_representations.groupby("service").indices:
                    kd_pred = kdt[service].predict(kdp.transform([bursts]))[0]
                    service_kd_pred.append(kd_pred)

                i_and_ell_t = np.concatenate([kd_pred[0] for kd_pred in service_kd_pred], axis=1)
                distances_t = np.concatenate([kd_pred[1] for kd_pred in service_kd_pred], axis=1)
                top_indices = np.argsort(distances_t, axis=1)[:, :50]

                i_and_ell = i_and_ell_t[np.arange(i_and_ell_t.shape[0])[:, np.newaxis], top_indices]
                distances = distances_t[np.arange(distances_t.shape[0])[:, np.newaxis], top_indices]
        else:
            i_and_ell = np.load(f"./precomputed/kd/{identifier}-i-and-ell.npy")
            distances = np.load(f"./precomputed/kd/{identifier}-distances.npy")

        cst = CandidateScoreTransformer()
        cs = CandidateSelector(setting.theta)
        out = cs.predict(cst.transform([(i_and_ell, distances)]))[0]

        # cs gives us representation indices, we need to convert them to video indices
        # this will wrongfully convert -1 values so we convert those back
        y_pred = df_videos.index.get_indexer(df_representations.iloc[out[:, 0]].video)
        y_pred[out[:, 0] == -1] = -1

        # offset due to requiring k+1 bursts
        y_true = df_videos.index.get_indexer(video_ids)[8:]
        y_times = times[8:]

        # similarly, as the strong attacker, we didn't query the overlap as the services
        # changed, so we need to remove the corresponding indices for the labels
        # to be aligned
        if model == Model.STRONG:
            for i, idx in enumerate(np.where(services[:-1] != services[1:])[0] + 1):
                idx_exp = np.s_[idx - 8 * (i + 1) : idx - 8 * i]
                y_true = np.delete(y_true, idx_exp)
                y_times = np.delete(y_times, idx_exp)

        label_starts = [
            (df_videos.index.get_loc(playback["video_id"]), playback["start_ts"])
            for playback in session["playbacks"]
        ]
        summary = session_summary(y_pred, y_times, y_true, label_starts)
        for i, (label, _) in enumerate(label_starts):
            identified = get_identified(summary[i], setting.time)
            misidentified = get_misidentified(summary[i])
            unknown = int(identified == 0 and misidentified == 0)

            df_results.iloc[row_idx] = [
                session["device"],
                df_videos.iloc[label].service,
                df_videos.index[label],
                identified,
                unknown,
                misidentified,
                summary[i, 0] if identified == 1 else np.nan,
            ]

            row_idx += 1

    print(f"\rEvaluating model '{model}' with method '{method}'... DONE", flush=True)

    result_name = f"{model}-{method}-{setting.theta}-{setting.time}"

    df_results.to_csv(f"./results/{result_name}.csv", index=False)
    print(f"Saved ./results/{result_name}.csv")

    df_aggregated = (
        df_results.groupby(["service"])
        .agg(
            {
                "identified": "sum",
                "unknown": "sum",
                "misidentified": "sum",
                "identified_time": "mean",
                "service": "count",
            },
        )
        .rename(columns={"service": "count"})
    )
    df_aggregated = df_aggregated.assign(
        accuracy=lambda x: x["identified"] / x["count"],
        precision=lambda x: x["identified"] / (x["identified"] + x["misidentified"]),
        recall=lambda x: x["identified"] / (x["identified"] + x["misidentified"] + x["unknown"]),
    )
    df_aggregated.to_csv(f"./results/{result_name}-aggregated.csv")
    print(f"Saved ./results/{result_name}-aggregated.csv")


def run_unknown(
    kdp: KDPointsTransformer, kdt: dict[str, KDCandidatePredictor], setting: Setting
) -> None:
    print("Evaluating model 'unknown'...", end="", flush=True)

    if setting.recompute:
        txt = create_bursts_unknown(setting.burstshark_path)
    else:
        txt = Path("./precomputed/bursts/unknown.txt")
    data = np.loadtxt(
        txt,
        dtype=[
            ("datasets", "U32"),
            ("applications", "U32"),
            ("bursts", "int32"),
        ],
    )
    datasets, applications, bursts = (
        data["datasets"],
        data["applications"],
        data["bursts"],
    )

    cst = CandidateScoreTransformer()
    results = ["dataset,application,score\n"]

    STEP = 1000  # Limit limit memory usage by querying in chunks.
    for i in range(0, bursts.shape[0], STEP):
        if setting.recompute:
            service_kd_pred = []
            for service in kdt:
                # Include the previous L bursts to account for the lag.
                kd_pred = kdt[service].predict(
                    kdp.transform([bursts[max(0, i - 75 - 8) : i + STEP]])
                )[0]
                service_kd_pred.append(kd_pred)

            i_and_ell_t = np.concatenate([kd_pred[0] for kd_pred in service_kd_pred], axis=1)
            distances_t = np.concatenate([kd_pred[1] for kd_pred in service_kd_pred], axis=1)
            top_indices = np.argsort(distances_t, axis=1)[:, :50]

            i_and_ell = i_and_ell_t[np.arange(i_and_ell_t.shape[0])[:, np.newaxis], top_indices]
            distances = distances_t[np.arange(distances_t.shape[0])[:, np.newaxis], top_indices]
        else:
            i_and_ell = np.load(f"./precomputed/kd/unknown-{i}-i-and-ell.npy")
            distances = np.load(f"./precomputed/kd/unknown-{i}-distances.npy")

        scores = cst.transform([(i_and_ell, distances)])[0][1]

        # Get the max score out of all candidates in each time step.
        max_per_step = np.max(scores, axis=1)
        if i > 0:
            max_per_step = max_per_step[75:]

        for j, score in enumerate(max_per_step):
            dataset = datasets[max(8, i) : i + STEP][j]
            application = applications[max(8, i) : i + STEP][j]
            results.append(f"{dataset},{application},{score}\n")

    print("\rEvaluating model 'unknown'... DONE", flush=True)

    with Path("./results/unknown-scores.csv").open("w") as f:
        f.writelines(results)
    print("Saved ./results/unknown-scores.csv")


def session_summary(
    y_pred: np.ndarray,
    y_times: np.ndarray,
    y_true: np.ndarray,
    label_starts: list[tuple[int, int]],
) -> np.ndarray:
    # summary columns:
    # 0. earliest occurence of correct label
    # 1. earliest occurence of incorrect label not associated with previous playback
    # 2. earliest occurence of incorrect label associated with previous playback
    # 3. last occurence of incorrect label
    summary = np.full((len(label_starts), 4), np.nan, dtype="float64")

    correct_mask = y_pred == y_true
    incorrect_mask = (y_pred != y_true) & (y_pred != -1)

    prev_label = -1
    for i, (label, playback_start) in enumerate(label_starts):
        label_mask = y_true == label
        correct_label_mask = label_mask & correct_mask
        incorrect_label_mask = label_mask & incorrect_mask

        if np.any(correct_label_mask):
            t = y_times[correct_label_mask].min()
            summary[i, 0] = t - playback_start

        if np.any(incorrect_label_mask):
            prev_label_mask = (y_pred == prev_label) & (prev_label != -1)

            incorrect_other = incorrect_label_mask & ~prev_label_mask
            incorrect_previous = incorrect_label_mask & prev_label_mask

            if np.any(incorrect_other):
                t = y_times[incorrect_other].min()
                summary[i, 1] = t - playback_start

            if np.any(incorrect_previous):
                t = y_times[incorrect_previous].min()
                summary[i, 2] = t - playback_start

            summary[i, 3] = y_times[incorrect_label_mask].max() - playback_start

        prev_label = label

    return summary


def get_identified(
    summary: np.ndarray,
    time: float,
    grace_period: float = 90.0,
) -> int:
    return int(
        ~np.isnan(summary[0])
        and (summary[0] <= time)
        and np.isnan(summary[1])
        and (np.isnan(summary[2]) or ((summary[3] <= grace_period) and (summary[0] > summary[3]))),
    )


def get_misidentified(
    summary: np.ndarray,
    grace_period: float = 90.0,
) -> int:
    return int(
        ~np.isnan(summary[1]) or (~np.isnan(summary[2]) and (summary[3] > grace_period)),
    )


if __name__ == "__main__":
    main()
