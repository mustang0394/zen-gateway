import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { Toaster } from "sonner";
import App from "./App";
import { useTheme } from "./hooks/use-theme";
import "./index.css";

// Toaster 的主题跟随当前生效的深浅色，避免浅色模式下弹出深色提示
function ThemedToaster() {
  const { effective } = useTheme();
  return <Toaster theme={effective} position="top-right" richColors />;
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 5_000 },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter basename="/admin">
        <App />
      </BrowserRouter>
      <ThemedToaster />
    </QueryClientProvider>
  </React.StrictMode>,
);
