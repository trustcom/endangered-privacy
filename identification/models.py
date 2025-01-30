from collections.abc import Iterable
from typing import Any, Self

import numpy as np
from sklearn.base import BaseEstimator, TransformerMixin
from sklearn.neighbors import KDTree


class KDPointsTransformer(BaseEstimator, TransformerMixin):
    def __init__(self, k: int = 8) -> None:
        self.k = k

        self._min = None
        self._max = None

    def fit(self, X: Iterable[np.ndarray], y: Any = None) -> Self:
        self._min = np.inf
        self._max = -np.inf

        for x in X:
            self._min = min(self._min, x.min())
            self._max = max(self._max, x.max())

        return self

    def transform(self, X: Iterable[np.ndarray]) -> np.ndarray:
        return np.fromiter(
            (self._transform_x(x) for x in X),
            dtype="O",
        )

    def _transform_x(self, x: np.ndarray) -> np.ndarray:
        x = x.astype("float64", copy=False)
        x = np.interp(x, (self._min, self._max), (0, 1))
        x = np.diff(x)

        return np.lib.stride_tricks.sliding_window_view(x, window_shape=(self.k,))


class KDCandidatePredictor(BaseEstimator):
    def __init__(self, tau: int = 50, leaf_size: int = 400) -> None:
        self.tau = tau

        # number of points at which to switch to brute-force
        # does not impact the results of a query
        self.leaf_size = leaf_size

        self._kd_tree = None
        self._mapped = None

    def fit(self, X: np.ndarray, y: np.ndarray | Iterable[int]) -> Self:
        self._mapped = np.vstack(
            np.fromiter(
                (self._map_points(x, idx) for x, idx in zip(X, y, strict=True)),
                dtype="O",
            ),
        )

        X = np.vstack(X)
        self._kd_tree = KDTree(X, leaf_size=self.leaf_size)

        return self

    def predict(self, X: np.ndarray, tau: int | None = None) -> np.ndarray:
        return np.fromiter(
            (self._query_tree(x, tau) for x in X),
            dtype="O",
        )

    def _query_tree(self, x: np.ndarray, tau: int | None = None) -> tuple[np.ndarray, np.ndarray]:
        if tau is None:
            tau = self.tau

        distances, ind = self._kd_tree.query(x, k=tau)
        i_and_ell = self._mapped[ind]

        return i_and_ell, distances

    def _map_points(self, x: np.ndarray, idx: np.uint32) -> np.ndarray:
        n_points = x.shape[0]

        return np.column_stack(
            (
                np.full(n_points, idx, dtype="uint32"),
                np.arange(n_points, dtype="uint32"),
            ),
        )


class CandidateScoreTransformer(BaseEstimator, TransformerMixin):
    def __init__(
        self,
        k: int = 8,
        L: int = 75,
        w: float = 3.0,
        lambda_score: float = 0.9,
        lambda_dist: float = 100.0,
        r: float = 0.025,
    ) -> None:
        self.k = k
        self.L = L
        self.w = w
        self.lambda_score = lambda_score
        self.lambda_dist = lambda_dist
        self.r = r

    def fit(self, X: np.ndarray, y: Any = None) -> Self:
        return self

    def transform(self, X: np.ndarray) -> np.ndarray:
        return np.fromiter(
            (self._compute_candidate_scores(x) for x in X),
            dtype="O",
        )

    def _compute_candidate_scores(
        self,
        x: tuple[np.ndarray, np.ndarray],
    ) -> tuple[np.ndarray, np.ndarray]:
        i_and_ell, distances = x
        i_and_ell = i_and_ell.astype("int32", copy=False)

        candidates, candidate_ind = self._compute_candidates(i_and_ell)

        base_scores = self._compute_base_scores(
            i_and_ell,
            distances,
            candidates,
            candidate_ind,
        )

        scores = self._compute_scores(base_scores)

        return candidates, scores

    def _compute_candidates(
        self,
        i_and_ell: np.ndarray,
    ) -> tuple[np.ndarray, np.ndarray]:
        i_and_ell[:, :, 1] -= np.arange(i_and_ell.shape[0])[:, np.newaxis]

        return np.unique(
            i_and_ell.reshape(-1, i_and_ell.shape[-1]),
            axis=0,
            return_inverse=True,
        )

    def _compute_base_scores(
        self,
        i_and_ell: np.ndarray,
        distances: np.ndarray,
        candidates: np.ndarray,
        candidate_ind: np.ndarray,
    ) -> np.ndarray:
        base_scores = np.full((i_and_ell.shape[0], candidates.shape[0]), np.nan)

        distances = np.where(distances > self.r, np.nan, distances)

        base_scores[
            np.arange(base_scores.shape[0])[:, np.newaxis],
            candidate_ind.reshape(base_scores.shape[0], -1),
        ] = np.exp(-self.lambda_dist * distances)

        return base_scores

    def _compute_scores(self, base_scores: np.ndarray) -> np.ndarray:
        scores = np.copy(base_scores)

        weights = np.ones(self.L)
        weights[: self.L - self.k - 1] = self.w

        for t in range(scores.shape[0]):
            current_row = scores[t]
            previous_row = scores[t - 1] if t > 0 else np.zeros(scores.shape[1])

            candidate_presence = ~np.isnan(current_row)

            if np.any(candidate_presence):
                aligned_weights = weights[max(0, self.L - 1 - t) :]
                present_scores = base_scores[max(0, t - self.L + 1) : t + 1][
                    :,
                    candidate_presence,
                ]

                current_row[candidate_presence] = np.log1p(
                    np.nansum(present_scores * aligned_weights[:, np.newaxis], axis=0),
                )

            if np.any(~candidate_presence):
                current_row[~candidate_presence] = (
                    previous_row[~candidate_presence] * self.lambda_score
                )

        return scores


class CandidateSelector(BaseEstimator):
    def __init__(self, theta: float = 2.2) -> None:
        self.theta = theta

    def fit(self, X: np.ndarray, y: Any = None) -> Self:
        return self

    def predict(self, X: np.ndarray) -> np.ndarray:
        return np.fromiter(
            (self._select_candidates(x) for x in X),
            dtype="O",
        )

    def _select_candidates(self, x: tuple[np.ndarray, np.ndarray]) -> np.ndarray:
        candidates, scores = x

        max_indices = np.argmax(scores, axis=1)
        max_scores = scores[np.arange(scores.shape[0]), max_indices]

        selected_indices = np.where(max_scores >= self.theta)[0]
        selected = np.full((scores.shape[0], 2), -1)

        selected[selected_indices] = np.column_stack(
            [
                candidates[max_indices[selected_indices], 0],
                candidates[max_indices[selected_indices], 1] + selected_indices,
            ],
        )

        return selected
