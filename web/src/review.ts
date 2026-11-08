import { useState } from "react";

/**
 * useReview tracks whether the draft with this key is being reviewed. Every edit gives a draft a
 * new key, so an edit made while the review is open closes it: what is sent is always what was
 * last reviewed.
 */
export function useReview(key: string) {
  const [reviewed, setReviewed] = useState<string | null>(null);
  return {
    open: reviewed === key,
    start: () => setReviewed(key),
    stop: () => setReviewed(null),
  };
}

export type Review = ReturnType<typeof useReview>;
