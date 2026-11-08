// How long a claim has left, for the claims panel.

export const second = 1000;
const minute = 60 * second;
const hour = 60 * minute;

// timeLeft says how long a claim has, in the console's language: seconds in its last minute,
// then minutes, then hours. It counts from this browser's clock, so a clock that is off moves
// the countdown by as much; the absolute time stays on the element. It returns null once the
// claim has expired, and undefined for a time it cannot read, which the row shows as given.
export function timeLeft(expiresAt: string, now: number, locale: string): string | null | undefined {
  const at = Date.parse(expiresAt);
  if (Number.isNaN(at)) {
    return undefined;
  }
  const left = at - now;
  if (left <= 0) {
    return null;
  }
  const format = new Intl.RelativeTimeFormat(locale, { numeric: "always" });
  if (left < minute) {
    return format.format(Math.ceil(left / second), "second");
  }
  if (left < hour) {
    return format.format(Math.floor(left / minute), "minute");
  }
  return format.format(Math.floor(left / hour), "hour");
}
