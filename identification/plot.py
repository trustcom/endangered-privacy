import argparse
import csv
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
from scipy.stats import norm

from models import CandidateScoreTransformer


def figure_5() -> None:
    IDENTIFIER = "427a7465-eead-4eef-ae6f-4e07ad614448"
    PLAYED = {
        2564459: "Max: Building Off the Grid S8E4 Container Dreamhouse",
        267876: "Amazon: Clarkson's Farm S2E5 Council-ing",
        2900635: "SVT: Fader Brown - The Numbers of the Beast",
    }

    data = np.loadtxt(
        f"./precomputed/bursts/{IDENTIFIER}.txt",
        dtype=[("times", "float64")],
        usecols=0,
    )
    times = data["times"]
    playback_times = times - times[0]

    i_and_ell = np.load(f"./precomputed/kd/{IDENTIFIER}-i-and-ell.npy")
    distances = np.load(f"./precomputed/kd/{IDENTIFIER}-distances.npy")
    alignments, scores = CandidateScoreTransformer().transform(
        [(i_and_ell, distances)]
    )[0]

    scores_for_played = {}
    for p, title in PLAYED.items():
        ind = np.where(alignments[:, 0] == p)[0]
        scores_for_played[title] = np.max(
            scores[:, ind].reshape(-1, ind.shape[0]), axis=1
        )

    plt.figure(figsize=(12, 6))

    for (title, score), color in zip(
        scores_for_played.items(), ("blue", "green", "red")
    ):
        plt.plot(
            playback_times,
            np.concatenate(
                (np.full(8, np.nan), np.where(score > 0.001, score, np.nan))
            ),
            label=title,
            color=color,
            linestyle="-",
            linewidth=2,
        )

    ind = np.where(~np.isin(alignments[:, 0], list(PLAYED.keys())))[0]
    scores_for_other = np.max(scores[:, ind].reshape(-1, ind.shape[0]), axis=1)
    plt.plot(
        playback_times,
        np.concatenate((np.full(8, np.nan), scores_for_other)),
        label="Other Videos (Max Score)",
        color="black",
        linestyle=":",
        linewidth=1,
    )

    seconds = list(range(1861))

    plt.grid(axis="y", linestyle=":", linewidth="0.5")
    plt.xlabel("Time (min:s)", fontsize=16)
    plt.ylabel("Score", fontsize=16)
    plt.xlim(0, len(seconds) - 1)
    plt.ylim((0, 6))
    plt.axhline(y=2.2, linestyle="--", linewidth=2, color="black")
    plt.text(x=120, y=2.25, s="θ = 2.2", fontsize=16, ha="center")

    tick_positions_doubled = np.arange(0, len(seconds), 120)
    x_labels = [
        f"{m}:{s:02d}" for m, s in zip(np.array(seconds) // 60, np.array(seconds) % 60)
    ]
    plt.xticks(
        tick_positions_doubled,
        (x_labels[i] for i in tick_positions_doubled),
        rotation=-45,
        ha="left",
        fontsize=16,
    )
    plt.yticks(fontsize=16)

    ax = plt.gca()
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.spines["left"].set_visible(False)
    ax.tick_params(axis="x", which="both")
    ax.tick_params(axis="y", which="both")
    ax.set_xticks(np.arange(0, len(seconds), 30), minor=True)
    ax.legend(
        fontsize=14,
        ncol=1,
        frameon=False,
        bbox_to_anchor=(0.68, 0.92),
        loc="right",
    )

    plt.savefig("./plots/figure-5.pdf", format="pdf", bbox_inches="tight")
    print("Saved ./plots/figure-5.pdf")


def figure_6() -> None:
    accuracies = {
        "strong-network": [],
        "weak-network": [],
        "weak-wifi": [],
    }

    for t in range(1, 601):
        for model in accuracies:
            with Path(f"./results/{model}-2.2-{t}.csv").open("r") as f:
                reader = csv.DictReader(f)
                identified, count = 0, 0
                for row in reader:
                    identified += row["identified"] == "1"
                    count += 1
                accuracies[model].append(identified / count)

    plt.figure(figsize=(8, 4))

    plt.plot(
        accuracies["strong-network"],
        color="red",
        linewidth=1.5,
        label="HTTPS",
    )
    plt.plot(
        accuracies["weak-network"],
        color="blue",
        linewidth=1.5,
        label="VPN",
    )
    plt.plot(
        accuracies["weak-wifi"],
        color="green",
        linewidth=1.5,
        label="VPN + Wi-Fi",
    )

    seconds = list(range(601))

    plt.grid(True, which="both", linestyle="--", linewidth=0.5)
    plt.xlabel("Playback Time (min:s)", fontsize=16)
    plt.ylabel("Accuracy", fontsize=16)
    plt.xlim(0, len(seconds) - 1)
    plt.ylim(0, 1.0)
    plt.legend(fontsize=16, loc="lower right", frameon=True)

    tick_positions_doubled = np.arange(0, len(seconds), 30)
    x_labels = [
        f"{m}:{s:02d}" for m, s in zip(np.array(seconds) // 60, np.array(seconds) % 60)
    ]
    plt.xticks(
        tick_positions_doubled,
        (x_labels[i] for i in tick_positions_doubled),
        rotation=-45,
        ha="left",
        fontsize=16,
    )
    plt.yticks(np.arange(0, 1.1, 0.1), fontsize=16)

    ax = plt.gca()
    ax.tick_params(axis="x", which="both")
    ax.tick_params(axis="y", which="both")

    for spine in plt.gca().spines.values():
        spine.set_visible(False)

    plt.savefig("./plots/figure-6.pdf", format="pdf", bbox_inches="tight")
    print("Saved ./plots/figure-6.pdf")


def figure_8() -> None:
    thresholds = np.arange(1.00, 5.05, 0.05)
    precisions = []
    recalls = []

    for theta in thresholds:
        identified, misidentified, unknown = 0, 0, 0
        with Path(f"./results/weak-network-{theta:.2f}-600.csv").open("r") as f:
            for row in csv.DictReader(f):
                identified += row["identified"] == "1"
                misidentified += row["misidentified"] == "1"
                unknown += row["unknown"] == "1"

        unknown_fp = 0
        with Path("./results/unknown-scores.csv").open("r") as f:
            for row in csv.DictReader(f):
                unknown_fp += float(row["score"]) >= theta

        precisions.append(
            identified / (identified + misidentified + unknown_fp)
            if identified + misidentified + unknown_fp > 0
            else 0
        )
        recalls.append(identified / (identified + misidentified + unknown))

    plt.figure(figsize=(8, 4))

    plt.plot(thresholds, precisions, label="Precision", color="green", linestyle="-")
    plt.plot(thresholds, recalls, label="Recall", color="blue", linestyle="-")

    plt.grid(True)
    plt.xlabel("Threshold (θ)", fontsize=16)
    plt.ylabel("Score", fontsize=16)
    plt.legend(
        fontsize=16, loc="upper right", bbox_to_anchor=(0.92, 0.32), frameon=True
    )

    ticks = np.arange(1.00, 5.05, 0.5)
    plt.xticks(ticks, (f"{tick:.1f}" for tick in ticks), fontsize=16)
    plt.yticks(np.arange(0, 1.1, 0.1), fontsize=16)

    for spine in plt.gca().spines.values():
        spine.set_visible(False)

    plt.savefig("./plots/figure-8.pdf", format="pdf", bbox_inches="tight")
    print("Saved ./plots/figure-8.pdf")


def _empirical_distribution(
    scores: list[float],
    bins: int = 100,
    xmin: int = 0,
    xmax: int = 2,
    ytarget: float = 1.225,
) -> list[float]:
    empirical_dist = [0] * bins

    for score in scores:
        if score < xmin or score > xmax:
            print("'False Positive Detected!!' because an out of bound score:", score)
        empirical_dist[int(bins * score / xmax)] += 1

    ymax = max(empirical_dist)
    return [ytarget * y / ymax for y in empirical_dist]


def _calculate_smooth_empirical_dist(
    filename: str, bins: int
) -> tuple[list[float], dict[str, list[float]], list[float]]:
    app_scores = {}

    with Path(filename).open("r") as file:
        lines = [line.split(",") for line in file.readlines()[1:]]
        apps = set(line[1] for line in lines)
        scores = [float(line[2]) for line in lines]

        for app in apps:
            app_scores[app] = [float(line[2]) for line in lines if line[1] == app]

    empirical_dist = _empirical_distribution(scores, bins)

    return scores, app_scores, empirical_dist


def _plot_normal(
    mean: float = 0.7, std_dev: float = 0.3, col: str = "red", spread: float = 0.06
) -> None:
    # Calculate the corresponding y values for the normal distribution
    x_values = np.linspace(-0.1, 2.3, 1000)
    y_values = norm.pdf(x_values, mean, std_dev)

    plt.plot(
        x_values,
        y_values,
        linestyle="dashed",
        label=f"Normal, mean {mean}, std. dev. {std_dev}",
        color=col,
    )

    mean2 = mean
    std_dev2 = round(std_dev - spread, 3)
    y_values2 = norm.pdf(x_values, mean2, std_dev2)

    mean3 = mean
    std_dev3 = round(std_dev + spread, 3)
    y_values3 = norm.pdf(x_values, mean3, std_dev3)

    plt.fill_between(
        x_values,
        y_values2,
        y_values3,
        color=col,
        label=f"Normal({mean3}, [{std_dev2}-{std_dev3}])",
        alpha=0.25,
    )


def _plot_empirical_score_distribution(
    filename: str = "./results/unknown-scores.csv",
    bins: int = 100,
    xmax: int = 2,
    plotfile: str = "./plots/scores.pdf",
    show_legend: bool = True,
    show_axlabels: bool = True,
    alt_title: str | None = None,
    ybounds: tuple[float, float] | None = None,
    xbounds: tuple[float, float] | None = None,
) -> None:
    plt.rcParams.update({"font.size": 14})

    scores, app_scores, empirical_dist = _calculate_smooth_empirical_dist(
        filename, bins
    )

    plt.figure(figsize=(10, 3.8))

    X = [xmax * x / bins for x in range(bins)]

    if ybounds:
        plt.ylim(ybounds)

    if xbounds:
        plt.xlim(xbounds)

    plt.plot(X, empirical_dist, label=f"Histogram all unknown ({bins} bins)")
    plt.plot(
        X,
        _empirical_distribution(app_scores["hbomax"], ytarget=2),
        label=f"Histogram HBOMax ({bins} bins)",
    )

    # Use heuristically calculated mean and standard deviation that approximate the empirical distribution
    hbomax_mean = round(np.mean(app_scores["hbomax"]) - 0.1, 3)
    hbomax_std = round(np.std(app_scores["hbomax"]) + 0.054, 3)

    # Plot the normal distribution
    _plot_normal(hbomax_mean, hbomax_std, "red", 0.05)

    all_mean = round(np.mean(scores) + 0.15, 3)
    all_std = round(np.std(scores) - 0.087, 3)

    _plot_normal(all_mean, all_std, "orange", 0.05)

    for spine in plt.gca().spines.values():
        spine.set_visible(False)

    if not alt_title:
        plt.title(
            f"Empirical Distribution of Scores ({round(len(scores) / 1000)}k datapoints, normalized)"
        )
    else:
        plt.title(alt_title)

    if show_axlabels:
        plt.xlabel("Value")
        plt.ylabel("Empirical Probability Density")

    if show_legend:
        plt.legend(frameon=False, prop={"weight": "bold", "size": 14})

    plt.grid(True)
    plt.tight_layout()

    plt.savefig(plotfile, bbox_inches="tight")


def figure_7() -> None:
    _plot_empirical_score_distribution(
        plotfile="./plots/figure-7.pdf", xbounds=(-0.1, 2.3), ybounds=(0, 2.65)
    )
    print("Saved ./plots/figure-7.pdf")


def figure_11() -> None:
    _plot_empirical_score_distribution(
        plotfile="./plots/figure-11.pdf",
        show_legend=True,
        show_axlabels=True,
        alt_title="Tail of the Empirical Distribution of Scores",
        ybounds=(0, 0.25),
        xbounds=(1.1, 2.3),
    )
    print("Saved ./plots/figure-11.pdf")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--figure",
        type=int,
        choices=(5, 6, 7, 8, 11),
        help="Figure to plot (default: all)",
    )
    args = parser.parse_args()

    Path("./plots").mkdir(exist_ok=True)

    try:
        if args.figure is None or args.figure == 5:
            figure_5()
        if args.figure is None or args.figure == 6:
            figure_6()
        if args.figure is None or args.figure == 7:
            figure_7()
        if args.figure is None or args.figure == 8:
            figure_8()
        if args.figure is None or args.figure == 11:
            figure_11()
    except FileNotFoundError as ex:
        print(ex)
        print("Run evaluation to generate results!")


if __name__ == "__main__":
    main()
