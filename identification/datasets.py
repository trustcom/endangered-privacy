import itertools
import subprocess
from collections import defaultdict
from pathlib import Path
from typing import Any

import numpy as np
import pandas as pd


def load_tsv_datasets(
    services: list[str] | None = None,
    min_duration: int | None = 60,
    load_durations: bool = False,
) -> tuple[pd.DataFrame, pd.DataFrame]:
    df_videos_all = []
    df_representations_all = []

    for video_dataset in sorted(Path("../datasets/tsv").glob("*_videos.tsv")):
        service, _ = video_dataset.stem.split("_")
        if services is not None and service not in services:
            continue

        df_videos = pd.read_csv(video_dataset, sep="\t")
        df_videos = df_videos.set_index("video")
        df_videos.insert(0, "service", service)

        if min_duration is not None:
            df_videos = df_videos[df_videos["duration"] >= min_duration]

        representation_dataset = video_dataset.with_stem(
            video_dataset.stem.replace("videos", "representations"),
        )

        cols = [0, 1, 2, 3, 4, 5, 6]
        if load_durations:
            cols.extend([7, 8])

        df_representations = pd.read_csv(representation_dataset, sep="\t", usecols=cols)
        df_representations.insert(0, "service", service)

        df_representations = df_representations[df_representations["video"].isin(df_videos.index)]

        df_representations["segment_sizes"] = df_representations["segment_sizes"].apply(
            lambda x: np.fromstring(x[1:-1], sep=",", dtype="uint32"),
        )

        if load_durations:
            df_representations["segment_durations"] = df_representations["segment_durations"].apply(
                lambda x: np.array(
                    [
                        np.fromstring(inner, sep=",", dtype="uint32")
                        for inner in x[2:-2].split("},{")
                    ],
                ),
            )

        df_videos_all.append(df_videos)
        df_representations_all.append(df_representations)

    df_videos = pd.concat(df_videos_all)
    df_representations = pd.concat(df_representations_all, ignore_index=True)

    return df_videos, df_representations


def create_bursts(
    session: dict[str, Any],
    burstshark_path: str = "/usr/bin/burstshark",
    wifi: bool = False,
) -> list[str]:
    IP = {
        "linux": "10.0.1.86",
        "mac": "10.0.1.44",
        "windows": "10.0.1.42",
    }

    MAC = {"mac": "6c:40:08:94:35:7a", "windows": "64:5d:86:d4:2a:01"}

    attack_method = "wifi" if wifi else "network"
    r = f"../datasets/pcaps/{session[f'{attack_method}_capture_file']}"
    Y = f"wlan.da == {MAC[session['device']]}" if wifi else f"ip.dst == {IP[session['device']]}"
    program_args = [
        burstshark_path,
        "-r",
        r,
        "-Y",
        Y,
        "--min-bytes",
        "50000",
    ]
    if wifi:
        program_args.append("--wlan")
    else:
        program_args.append("-a")

    completed = subprocess.run(program_args, capture_output=True, text=True)

    if completed.returncode != 0:
        raise Exception(f'Burstshark error: "{completed.stderr.strip()}"')

    lines = completed.stdout.strip().split("\n")

    # Group by source address. In the weak attacker model this does
    # nothing as there is only one IP address (the address of the VPN)
    # or the MAC address of the transmitting device.
    burst_lines = defaultdict(list)
    for src, it in itertools.groupby(lines, key=lambda x: x.split()[2]):
        burst_lines[src].extend(it)

    services = [playback["service_id"] for playback in session["playbacks"]]
    navigation_service = services[0]

    result = []
    for burst_line in itertools.chain(*burst_lines.values()):
        cols = burst_line.split()
        burst_start_ts, burst_end_ts, burst_size = cols[6], cols[7], cols[10]

        # Label each burst with the correct service
        # and video being played for evaluation.
        for playback in session["playbacks"]:
            if (
                float(burst_start_ts) >= playback["start_ts"]
                and float(burst_end_ts) <= playback["end_ts"]
            ):
                navigation_service = services[min(services.index(playback["service_id"]) + 1, 2)]
                line = (
                    f"{burst_end_ts} {burst_size} {playback['service_id']} {playback['video_id']}\n"
                )
                break
        else:
            # Navigating service before playback.
            line = f"{burst_end_ts} {burst_size} {navigation_service} -\n"

        result.append(line)

    return result


def create_bursts_unknown(burstshark_path: str = "/usr/bin/burstshark") -> list[str]:
    result = []

    for dir_ in sorted(Path("../datasets/unknown-pcaps").iterdir(), key=lambda x: x.name.lower()):
        dataset = dir_.name.split("_")[0].lower()

        for f in sorted(dir_.iterdir()):
            capture_type, application, _ = f.stem.split("_", 2)
            program_args = [
                burstshark_path,
                "-r",
                f,
            ]
            if capture_type == "wifi":
                program_args.append("--wlan")
            else:
                program_args.append("-a")

            completed = subprocess.run(program_args, capture_output=True, text=True)

            if completed.returncode != 0:
                raise Exception(f'Burstshark error: "{completed.stderr.strip()}"')

            lines = completed.stdout.strip().split("\n")

            # Since we do not know any addresses in this scenario, for each
            # conversation we assume the host that is sending the most data is
            # the potential "video"-server. This is a reasonable assumption as
            # clients send only a fraction of the data during ABR streaming.
            conversations = set()
            bursts = defaultdict(list)
            for line in lines:
                cols = line.split()
                src, dst, burst_size = cols[2], cols[4], cols[10]
                bursts[(src, dst)].append(int(burst_size))
                conversations.add(tuple(sorted([src, dst])))

            for a1, a2 in conversations:
                up, down = bursts[(a1, a2)], bursts[(a2, a1)]
                max_bursts = up if sum(up) > sum(down) else down
                result.extend(
                    f"{dataset} {application} {burst}\n" for burst in max_bursts if burst > 50000
                )

    return result
