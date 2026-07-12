function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var obj={x:10,y:20};var r;with(obj){r=x+y;}check(r,30);with(obj){x=100;}check(obj.x,100);console.log('PASS');
