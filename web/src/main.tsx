import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes } from "react-router";
import Download from "./routes/Download";
import Enroll from "./routes/Enroll";
import Login from "./routes/Login";

export function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/download" element={<Download />} />
        <Route path="/enroll" element={<Enroll />} />
        <Route path="*" element={<Navigate to="/download" replace />} />
      </Routes>
    </BrowserRouter>
  );
}

const container = document.getElementById("root");
if (container) {
  createRoot(container).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
