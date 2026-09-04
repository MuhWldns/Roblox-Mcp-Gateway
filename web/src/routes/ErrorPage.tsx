import { Link, useNavigate, useRouteError } from "react-router";

function describe(error: unknown): string {
  // The browser API client only embeds HTTP status codes in its error
  // messages, so surfacing the message cannot disclose secrets.
  if (error instanceof Error && error.message) {
    return error.message;
  }
  return "An unexpected error occurred while loading this page.";
}

export default function ErrorPage() {
  const error = useRouteError();
  const navigate = useNavigate();

  return (
    <main>
      <section data-testid="error-page" role="alert">
        <h1>Something went wrong</h1>
        <p>{describe(error)}</p>
      </section>
      <p role="status">Your session and data are safe. Nothing was changed.</p>
      <button type="button" onClick={() => navigate("/")}>
        Try again
      </button>
      <Link to="/login">Back to sign in</Link>
    </main>
  );
}
