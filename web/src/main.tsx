import { StrictMode, Suspense } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { ThemeProvider } from 'next-themes'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
// sonner also injects these styles at runtime, which the console's CSP (style-src 'self') blocks;
// imported, they ship in our own stylesheet.
import 'sonner/dist/styles.css'
import './index.css'
import AppRoutes from './routes.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem>
      <TooltipProvider delayDuration={200}>
        <BrowserRouter>
          <Suspense fallback={null}>
            <AppRoutes />
          </Suspense>
        </BrowserRouter>
        <Toaster richColors closeButton />
      </TooltipProvider>
    </ThemeProvider>
  </StrictMode>,
)
