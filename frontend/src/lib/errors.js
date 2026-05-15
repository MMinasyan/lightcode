export function errorText(err) {
  if (!err) return 'unknown error';
  if (typeof err === 'string') return err;
  if (err.message) return err.message;
  if (err.toString) return err.toString();
  return String(err);
}
