import Providers from './components/providers/Providers';
import Layout from './components/layout/Layout';
import MainPage from './components/main-page/MainPage';

function App() {
  return (
    <Providers>
      <Layout>
        <MainPage />
      </Layout>
    </Providers>
  );
}

export default App;
