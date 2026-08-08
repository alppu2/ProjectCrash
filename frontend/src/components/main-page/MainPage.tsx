import PacketSender from '../packet-sender/PacketSender';
import ChatPanel from '../chat/ChatPanel';
import './MainPage.css';

function MainPage() {
  return (
    <div className="main-layout">
      <PacketSender />
      <ChatPanel />
    </div>
  );
}

export default MainPage;
